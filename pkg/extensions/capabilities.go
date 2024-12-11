package extensions

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/microsoft/usvc-apiserver/pkg/osutil"
)

// Keep in sync with https://github.com/microsoft/usvc-apiserver/schemas/v1.0/capabilities.json
type ExtensionCapability string

const (
	// The extension is a controller host.
	ControllerCapability ExtensionCapability = "controller"

	// The extension is a workload renderer.
	WorkloadRendererCapability ExtensionCapability = "workload-renderer"

	// The extension is the API server (there can be only one).
	ApiServerCapability ExtensionCapability = "api-server"

	// The extension accepts --monitor flag for parent process monitoring.
	// This implies it should not be terminated when DCP is shutting down; it will exit on its own when DCP process exist.
	// This allows it to perform a graceful shutdown.
	ProcessMonitorCapability ExtensionCapability = "process-monitor"
)

type ExtensionCapabilities struct {
	Name         string                `json:"name"` // Name of the extension, for error reporting and similar purposes.
	Id           string                `json:"id"`   // Unique ID of the extension, primarily for referring to the extension via command-line arguments.
	Capabilities []ExtensionCapability `json:"capabilities"`
}

func WriteCapabiltiesDoc(w io.Writer, extName string, id string, caps []ExtensionCapability) error {
	cdoc := ExtensionCapabilities{
		Name:         extName,
		Id:           id,
		Capabilities: caps,
	}

	output, err := json.Marshal(cdoc)
	if err != nil {
		return fmt.Errorf("unexpected error ocurred while creating JSON document describing capabilities of this program: %w", err)
	}

	_, err = w.Write(osutil.WithNewline(output))
	if err != nil {
		return fmt.Errorf("unexpected error ocurred while writing JSON document describing capabilities of this program: %w", err)
	}

	return nil
}
