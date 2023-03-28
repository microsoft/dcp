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
)

type DcpExtension struct {
	Name         string
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
			return filepath.SkipDir
		}

		// This will interrogate each extension serially. If we have a lot of extensions,
		// we may want to parallelise this.

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
		formattedOutput := ""
		if stdout.Len() > 0 {
			formattedOutput = "\n" + stdout.String()
		}
		if stderr.Len() > 0 {
			formattedOutput += "\n" + stderr.String()
		}
		if err != nil {
			return DcpExtension{}, fmt.Errorf("could not determine capabilities of extension '%s': %w%s", path, err, formattedOutput)
		}
		if code != 0 {
			return DcpExtension{}, fmt.Errorf("could not determine capabilities of extension '%s': exit code %d%s", path, code, formattedOutput)
		}
	}

	var caps extensions.ExtensionCapabilities
	err = json.Unmarshal(stdout.Bytes(), &caps)
	if err != nil {
		return DcpExtension{}, fmt.Errorf("extension capabilities response ('%s') is invalid: %w", stdout.String(), err)
	}

	return DcpExtension{
		Name:         caps.Name,
		Path:         path,
		Capabilities: caps.Capabilities,
	}, nil
}
