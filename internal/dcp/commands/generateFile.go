package commands

import "github.com/spf13/cobra"

func NewGenerateFileCommand() *cobra.Command {
	generateFileCmd := &cobra.Command{
		Use:   "generate-file",
		Short: "Generate file from a template.",
		Long: `Generate file from a template.

Use this command to process a template and save the resulting output to a file, for example:

    dcp generate-file --template local.env.template   # Produces "local.env" file.
	
By default it is assumed that the template file ends up with ".template", e.g. "myfile.template", resulting in "myfile" output file. 
If other naming convention is desired, use --output option.

The template file uses Go text templates syntax: https://pkg.go.dev/text/template
Additional available functions are:

  - randomPassword lowerCase upperCase digits symbols
	where
	  lowerCase is the number of lowercase letters in the password
	  upperCase is the number of uppercase letters in the password
	  digits is the number of digits (0-9) in the password
	  symbols is the number of symbol characters in the password
	Default values for parameters are 8, 8, 4, and 0.`,
		RunE: generateFile,
	}

	// TODO: --output option
	// TODO: --overwrite option
	return generateFileCmd
}

func generateFile(cmd *cobra.Command, args []string) error {
	// TODO: implement
	return nil
}
