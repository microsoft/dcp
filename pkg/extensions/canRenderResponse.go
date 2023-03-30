package extensions

// Keep in sync with https://github.com/usvc-dev/apiserver/schemas/v1.0/can-render.json
type CanRenderResult string

const (
	ResultYes CanRenderResult = "yes"
	ResultNo  CanRenderResult = "no"
)

type CanRenderResponse struct {
	Result CanRenderResult `json:"result"`
	Reason string          `json:"reason,omitempty"` // Must be provided if result is "no".
}
