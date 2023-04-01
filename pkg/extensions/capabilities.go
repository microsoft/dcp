package extensions

// Keep in sync with https://github.com/usvc-dev/apiserver/schemas/v1.0/capabilities.json
type ExtensionCapability string

const (
	ControllerCapability       ExtensionCapability = "controller"
	WorkloadRendererCapability ExtensionCapability = "workload-renderer"
)

type ExtensionCapabilities struct {
	Name         string                `json:"name"` // Name of the extension, for error reporting and similar purposes.
	Id           string                `json:"id"`   // Unique ID of the extension, primarily for referring to the extension via command-line arguments.
	Capabilities []ExtensionCapability `json:"capabilities"`
}
