//go:build !windows

package osutil

import (
	"os"
)

func IsAdmin() (bool, error) {
	if os.Getuid() == 0 {
		return true, nil
	} else {
		return false, nil
	}
}
