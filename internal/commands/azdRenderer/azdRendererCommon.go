package azdRenderer

import (
	"github.com/spf13/cobra"
)

type rendererData struct {
	appRootDir string
}

const (
	// Flag names
	appRootDirFlag      = "root-dir"
	appRootDirFlagShort = "r"
)

func addRootDirFlag(cmd *cobra.Command, dest *string) {
	cmd.Flags().StringVarP(dest, appRootDirFlag, appRootDirFlagShort, "", "The root directory of the application to be rendered")
}
