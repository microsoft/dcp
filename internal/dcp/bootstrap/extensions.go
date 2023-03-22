package bootstrap

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/usvc-dev/stdtypes/pkg/process"
)

type ExtensionRole uint32

const (
	ControllerRole ExtensionRole = 0x1
	RendererRole   ExtensionRole = 0x2
)

type DcpExtension struct {
	Path string
	Role ExtensionRole
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

func GetExtensions(ctx context.Context) ([]DcpExtension, error) {
	extDir, err := GetExtensionsDir()
	if err != nil {
		return nil, err
	}

	extensions := []DcpExtension{}
	pe := process.NewOSExecutor()

	err = filepath.Walk(extDir, func(path string, info os.FileInfo, err error) error {
		// err means some path was discovered, but is not accessible
		if err != nil {
			return err
		}

		if info.IsDir() {
			return filepath.SkipDir
		}

		if (info.Mode().Perm() & UserExecute) != 0 {
			cmd := exec.CommandContext(ctx, path, "get-capabilities")
			code, err := process.Run(ctx, pe, cmd)

			// TODO: capture stdout and stderr, add stderr to error message as necessary
			if err != nil {
				return fmt.Errorf("could not determine capabilities of extension '%s': %w", path, err)
			}
			if code != 0 {
				return fmt.Errorf("could not determine capabilities of extension '%s': exit code %d", path, code)
			}

			// TODO: parse stdout to determine extension role
		}
	})
	if err != nil {
		return nil, fmt.Errorf("could not determine installed DCP extensions: %w", err)
	}
}
