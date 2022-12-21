package commands

import (
	"fmt"
	"io"
	"os"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/usvc-dev/apiserver/internal/osutil"
)

type generateFileFlagData struct {
	input     string
	output    string
	overwrite bool
}

var generateFileFlags generateFileFlagData

const (
	inputFlag       = "input"
	inputFlagShort  = "i"
	outputFlag      = "output"
	outputFlagShort = "o"
	overwriteFlag   = "overwrite"
)

func NewGenerateFileCommand() *cobra.Command {
	generateFileCmd := &cobra.Command{
		Use:   "generate-file",
		Short: "Generate file from a template.",
		Long: `Generate file from a template.

Use this command to process a template and save the resulting output to a file, for example:

    dcp generate-file --input local.env.template --output local.env
	
If --input parameter is missing, the template content will be read from standard input.
If --output parameter is missing, the resulting content will be written to standard output.

The template file uses Go text templates syntax: https://pkg.go.dev/text/template
Additional functions that can be used inside the template are:

  randomPassword lowerCase upperCase digits symbols
	Parameters:
	  lowerCase is the number of lowercase letters in the password
	  upperCase is the number of uppercase letters in the password
	  digits is the number of digits (0-9) in the password
	  symbols is the number of symbol characters in the password
	Default values for parameters are 8, 8, 4, and 0.`,

		RunE: generateFile,
		Args: cobra.NoArgs,
	}

	generateFileCmd.LocalFlags().StringVarP(&generateFileFlags.input, inputFlag, inputFlagShort, "", "Name of the file containing the template for file generation. If omitted, the template will be read from standard input.")
	generateFileCmd.LocalFlags().StringVarP(&generateFileFlags.output, outputFlag, outputFlagShort, "", "Output file name. If omitted, standard output will be used.")
	generateFileCmd.LocalFlags().BoolVar(&generateFileFlags.overwrite, overwriteFlag, false, "If present, and the output file exists already, the file will be truncated and overwritten.")

	return generateFileCmd
}

func generateFile(cmd *cobra.Command, args []string) error {
	var err error
	var input *os.File
	if inputFileName := generateFileFlags.input; inputFileName != "" {
		input, err = openInputFile(inputFileName)
		if err != nil {
			return err
		} else {
			defer input.Close()
		}
	} else {
		input = os.Stdin
	}

	var output *os.File
	if outputFileName := generateFileFlags.output; outputFileName != "" {
		output, err = openOrCreateOutputFile(outputFileName, osutil.PermissionFileOwnerOnly)
		if err != nil {
			return err
		} else {
			defer output.Close()
		}
	} else {
		output = os.Stdout
	}

	contentBytes, err := io.ReadAll(input)
	if err != nil {
		return fmt.Errorf("template file could not be read: %w", err)
	}

	t, err := template.New("content").Parse(string(contentBytes))
	if err != nil {
		return fmt.Errorf("the template could not be parsed: %w", err)
	}

	// TODO: add randomPassword function to the template context

	err = t.Execute(output, nil)
	if err != nil {
		return fmt.Errorf("the file could not be generated: %w", err)
	}

	return nil
}

func openInputFile(fileName string) (*os.File, error) {
	fi, err := os.Stat(fileName)
	if err != nil {
		return nil, fmt.Errorf("inaccessible template file: %w", err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("the value of --input parameter points to a directory--file was expected")
	}

	input, err := os.OpenFile(fileName, os.O_RDONLY, osutil.PermissionFile)
	if err != nil {
		return nil, fmt.Errorf("template file could not be opened: %w", err)
	}

	return input, nil
}

func openOrCreateOutputFile(fileName string, newFilePerm os.FileMode) (*os.File, error) {
	fopenFlags := os.O_WRONLY
	var errMsg string

	if generateFileFlags.overwrite {
		fopenFlags = fopenFlags | os.O_TRUNC | os.O_CREATE // Create if necessary, otherwise truncate.
		errMsg = "output file could not be opened: %w"
	} else {
		fopenFlags = fopenFlags | os.O_EXCL | os.O_CREATE // Fail if file already exists.
		errMsg = "output file could not be created: %w"
	}

	// The permissions are really only used
	output, err := os.OpenFile(fileName, fopenFlags, newFilePerm)
	if err != nil {
		return nil, fmt.Errorf(errMsg, err)
	}

	return output, nil
}
