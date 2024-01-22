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
	ControllerCapability       ExtensionCapability = "controller"
	WorkloadRendererCapability ExtensionCapability = "workload-renderer"
	ApiServerCapability        ExtensionCapability = "api-server"
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
