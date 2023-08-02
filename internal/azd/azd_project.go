package azd

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// CONSIDER for early development purposes this file contains some duplicated and modified
// Azure Developer CLI data structures for describing AZD-enabled applications.
// Eventually we probably want to consume the real AZD data structures instead,
// as defined by their packages, primarily
// github.com/Azure/azure-dev/cli/azd/pkg/project

type ProjectConfig struct {
	Name     string                    `yaml:"name"`
	Services map[string]*ServiceConfig `yaml:"services,omitempty"`
}

type HostType string

const (
	HostTypeAppService   HostType = "appservice"
	HostTypeContainerApp HostType = "containerapp"
	HostTypeFunction     HostType = "function"
	HostTypeStaticWebApp HostType = "staticwebapp"
	HostTypeMsSQL        HostType = "mssql"
)

type ProgrammingLanguage string

const (
	ProgrammingLanguageDotNet ProgrammingLanguage = "dotnet"
	ProgrammingLanguagePython ProgrammingLanguage = "python"
	ProgrammingLanguageNodeJS ProgrammingLanguage = "js"
	ProgrammingLanguageJava   ProgrammingLanguage = "java"
)

type ServiceConfig struct {
	// The friendly name/key of the project from the azure.yaml file
	Name string `yaml:"name"`
	// The relative path to the project folder from the project root
	RelativePath string `yaml:"project"`
	// The azure hosting model to use, ex) appservice, function, containerapp
	Host HostType `yaml:"host"`
	// The programming language of the project
	Language ProgrammingLanguage `yaml:"language"`
}

func ParseProjectConfig(yamlContent []byte) (*ProjectConfig, error) {
	var projectConfig ProjectConfig

	if err := yaml.Unmarshal(yamlContent, &projectConfig); err != nil {
		return nil, fmt.Errorf("unable to parse azure.yaml file: %w", err)
	}

	for key, svc := range projectConfig.Services {
		svc.Name = key

		switch svc.Language {
		case "", "dotnet", "csharp", "fsharp":
			svc.Language = ProgrammingLanguageDotNet
		case "py", "python":
			svc.Language = ProgrammingLanguagePython
		case "js", "ts":
			svc.Language = ProgrammingLanguageNodeJS
		case "java":
			svc.Language = ProgrammingLanguageJava
		}
	}

	return &projectConfig, nil
}
