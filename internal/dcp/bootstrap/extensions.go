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

	"github.com/usvc-dev/apiserver/pkg/extensions"
	"github.com/usvc-dev/stdtypes/pkg/process"
	"github.com/usvc-dev/stdtypes/pkg/slices"
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
	processExecutor = process.NewOSExecutor()
)

func GetExtensions(ctx context.Context) ([]DcpExtension, error) {
	extDir, err := GetExtensionsDir()
	if err != nil {
		return nil, err
	}

	extensions := []DcpExtension{}

	err = filepath.Walk(extDir, func(path string, info os.FileInfo, err error) error {
		// err means some path was discovered, but is not accessible
		if err != nil {
			return err
		}

		if info.IsDir() {
			if path != extDir {
				// We don't want to recurse into subdirectories
				return filepath.SkipDir
			} else {
				// We don't want to process the extensions directory itself
				return nil
			}
		}

		// This will interrogate each extension serially. If we have a lot of extensions,
		// we may want to parallelise this (e.g. using MapConcurrent()).

		if (info.Mode().Perm() & UserExecute) != 0 {
			ext, err := getExtensionCapabilities(ctx, path)
			if err != nil {
				return err
			}

			extensions = append(extensions, ext)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("could not determine installed DCP extensions: %w", err)
	}

	return extensions, nil
}

func getExtensionCapabilities(ctx context.Context, path string) (DcpExtension, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "get-capabilities")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code, err := process.Run(ctx, processExecutor, cmd)

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

func (ext *DcpExtension) CanRender(ctx context.Context, appRootDir string) (extensions.CanRenderResponse, error) {
	if !slices.Contains(ext.Capabilities, extensions.WorkloadRendererCapability) {
		return extensions.CanRenderResponse{}, fmt.Errorf("extension '%s' is not a workload renderer", ext.Id)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ext.Path, "can-render")
	cmd.Args = append(cmd.Args, "--root-dir", appRootDir) // append because the first argument is the executable path
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	code, err := process.Run(ctx, processExecutor, cmd)

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

func (ext *DcpExtension) Render(ctx context.Context, appRootDir string, kubeconfigPath string) error {
	if !slices.Contains(ext.Capabilities, extensions.WorkloadRendererCapability) {
		return fmt.Errorf("extension '%s' is not a workload renderer", ext.Id)
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ext.Path, "render-workload")
	cmd.Args = append(cmd.Args,
		"--root-dir", appRootDir,
		"--kubeconfig", kubeconfigPath,
	) // append because the first argument is the executable path
	cmd.Stderr = &stderr
	code, err := process.Run(ctx, processExecutor, cmd)

	if err != nil || code != 0 {
		return getCommandExecutionError("could not application", nil, &stderr, err, code)
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
