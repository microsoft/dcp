package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/usvc-apiserver/internal/dcppaths"
	"github.com/microsoft/usvc-apiserver/pkg/extensions"
	"github.com/microsoft/usvc-apiserver/pkg/process"
	"github.com/microsoft/usvc-apiserver/pkg/slices"
)

type DcpExtension struct {
	Name         string
	Id           string
	Path         string
	Capabilities []extensions.ExtensionCapability
}

type FilePermission uint32

const (
	UserRead fs.FileMode = 1 << (8 - iota)
	UserWrite
	UserExecute
	GroupRead
	GroupWrite
	GroupExecute
	OtherRead
	OtherWrite
	OtherExecute
)

var (
	// File basename -> extension
	WellKnownExtensions = map[string]DcpExtension{
		"dcpctrl": {
			Name:         "DCP controller host",
			Id:           "dcpctrl",
			Capabilities: []extensions.ExtensionCapability{extensions.ControllerCapability, extensions.ProcessMonitorCapability},
		},
	}
)

func GetExtensions(ctx context.Context, log logr.Logger) ([]DcpExtension, error) {
	extDirs, err := dcppaths.GetExtensionsDirs()
	if err != nil {
		return nil, err
	}

	extensions := []DcpExtension{}

	for _, extDir := range extDirs {
		// Evaluate symlinks for the directory
		realExtDir, evalErr := filepath.EvalSymlinks(extDir)
		if evalErr != nil {
			if runtime.GOOS == "windows" {
				log.Error(evalErr, "failed to evaluate symlinks for directory; note: directory junctions are not supported, use directory symbolic link instead", "directory", extDir)
			} else {
				log.Error(evalErr, "failed to evaluate symlinks for directory", "directory", extDir)
			}
			continue
		}

		err = filepath.Walk(realExtDir, func(path string, info os.FileInfo, err error) error {
			// err means some path was discovered, but is not accessible
			if err != nil {
				return err
			}

			if info.IsDir() {
				if path != realExtDir {
					// We don't want to recurse into subdirectories
					return filepath.SkipDir
				} else {
					// We don't want to process the extensions directory itself
					return nil
				}
			}

			// The following will interrogate each extension serially. If we have a lot of extensions,
			// we may want to parallelize this (e.g. using MapConcurrent()).

			isExe := false
			if runtime.GOOS == "windows" {
				isExe = filepath.Ext(path) == ".exe"
			} else {
				isExe = info.Mode().Perm()&UserExecute != 0
			}
			if isExe {
				ext, capabilityQueryErr := getExtensionCapabilities(ctx, path, log)
				if capabilityQueryErr != nil {
					return capabilityQueryErr
				}

				extensions = append(extensions, ext)
			}

			return nil
		})
	}
	if err != nil {
		return nil, fmt.Errorf("could not determine installed DCP extensions: %w", err)
	}

	return extensions, nil
}

func getExtensionCapabilities(ctx context.Context, path string, log logr.Logger) (DcpExtension, error) {
	processExecutor := process.NewOSExecutor(log.WithName("extensions"))
	defer processExecutor.Dispose()
	if expandedPath, err := filepath.EvalSymlinks(path); err == nil {
		// We will just do the get-capabilities call (slow path) if EvalSymlinks() fails.
		exeName := filepath.Base(expandedPath)
		ext := filepath.Ext(exeName)
		if ext != "" && len(ext) < len(exeName) {
			exeName = exeName[:len(exeName)-len(ext)]
		}

		if ext, found := WellKnownExtensions[exeName]; found {
			ext.Path = expandedPath
			return ext, nil
		}
	}

	// Give an extension up to 10 seconds to respond to a capabilities request
	timeoutCtx, cancelTimeoutCtx := context.WithTimeout(ctx, 10*time.Second)
	defer cancelTimeoutCtx()

	cmd := exec.Command(path, "get-capabilities")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code, err := process.RunToCompletion(timeoutCtx, processExecutor, cmd)

	if err != nil || code != 0 {
		return DcpExtension{}, getCommandExecutionError(fmt.Sprintf("could not determine capabilities of extension '%s'", path), &stdout, &stderr, err, code)
	}

	var caps extensions.ExtensionCapabilities
	err = json.Unmarshal(stdout.Bytes(), &caps)
	if err != nil {
		return DcpExtension{}, fmt.Errorf("extension capabilities response ('%s') is invalid: %w", stdout.String(), err)
	}

	return DcpExtension{
		Name:         caps.Name,
		Id:           caps.Id,
		Path:         path,
		Capabilities: caps.Capabilities,
	}, nil
}

func (ext *DcpExtension) CanRender(ctx context.Context, appRootDir string, log logr.Logger) (extensions.CanRenderResponse, error) {
	processExecutor := process.NewOSExecutor(log.WithName(ext.Name).WithName("can-render"))
	defer processExecutor.Dispose()
	if !slices.Contains(ext.Capabilities, extensions.WorkloadRendererCapability) {
		return extensions.CanRenderResponse{}, fmt.Errorf("extension '%s' is not a workload renderer", ext.Id)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(ext.Path, "can-render")
	cmd.Args = append(cmd.Args, "--root-dir", appRootDir) // append because the first argument is the executable path
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code, err := process.RunToCompletion(ctx, processExecutor, cmd)

	if err != nil || code != 0 {
		return extensions.CanRenderResponse{}, getCommandExecutionError(fmt.Sprintf("could not determine if application type '%s' (%s) can be run", ext.Id, ext.Name), &stdout, &stderr, err, code)
	}

	var canRenderResponse extensions.CanRenderResponse
	err = json.Unmarshal(stdout.Bytes(), &canRenderResponse)
	if err != nil {
		return extensions.CanRenderResponse{}, fmt.Errorf("a test whether application type '%s' (%s) can be run resulted in invalid response: %w", ext.Id, ext.Name, err)
	}

	return canRenderResponse, nil
}

func (ext *DcpExtension) Render(ctx context.Context, appRootDir string, kubeconfigPath string, log logr.Logger) error {
	processExecutor := process.NewOSExecutor(log.WithName(ext.Name).WithName("render"))
	defer processExecutor.Dispose()
	if !slices.Contains(ext.Capabilities, extensions.WorkloadRendererCapability) {
		return fmt.Errorf("extension '%s' is not a workload renderer", ext.Id)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(ext.Path, "render-workload")
	cmd.Args = append(cmd.Args,
		"--root-dir", appRootDir,
		"--kubeconfig", kubeconfigPath,
	) // append because the first argument is the executable path
	cmd.Stderr = &stderr
	code, err := process.RunToCompletion(ctx, processExecutor, cmd)

	if err != nil || code != 0 {
		return getCommandExecutionError("could not run application", nil, &stderr, err, code)
	}
	return nil
}

func getCommandExecutionError(prefix string, stdout *bytes.Buffer, stderr *bytes.Buffer, err error, exitCode int32) error {
	formattedOutput := ""
	if stdout != nil && stdout.Len() > 0 {
		formattedOutput = "\n" + stdout.String()
	}
	if stderr != nil && stderr.Len() > 0 {
		formattedOutput += "\n" + stderr.String()
	}
	if err != nil {
		return fmt.Errorf("%s: %w%s", prefix, err, formattedOutput)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s: exit code %d%s", prefix, exitCode, formattedOutput)
	}
	panic("there should be an error, or exit code should be non-zero") // this should never happen
}
