package main

import (
	"fmt"
	"os"

	"github.com/usvc-dev/apiserver/internal/dcp/commands"
)

func main() {
	root := commands.NewRootCmd()
	err := root.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
