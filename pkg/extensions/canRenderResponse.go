package extensions

import (
	"fmt"
)

// Keep in sync with https://github.com/usvc-dev/apiserver/schemas/v1.0/can-render.json
type CanRenderResult string

const (
	CanRenderResultYes CanRenderResult = "yes"
	CanRenderResultNo  CanRenderResult = "no"
)

type CanRenderResponse struct {
	Result CanRenderResult `json:"result"`
	Reason string          `json:"reason,omitempty"` // Must be provided if result is "no".
}

func (c CanRenderResponse) IsValid() error {
	if c.Result != CanRenderResultYes && c.Result != CanRenderResultNo {
		return fmt.Errorf("result must be either 'yes' or 'no'")
	}
	if c.Result == CanRenderResultNo && c.Reason == "" {
		return fmt.Errorf("reason must be provided if result is 'no'")
	}
	return nil
}
