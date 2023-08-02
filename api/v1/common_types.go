package v1

// EnvVar represents an environment variable present in a Container or Executable.
// +k8s:openapi-gen=true
type EnvVar struct {
	// Name of the environment variable
	Name string `json:"name"`

	// Value of the environment variable. Defaults to "" (empty string).
	// +optional
	Value string `json:"value,omitempty"`
	// CONSIDER allowing expansion of existing variable references e.g. using ${VAR_NAME} syntax and $$ to escape the $ sign
}
