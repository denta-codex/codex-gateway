// Package example shows the adapter contract without being registered in the
// production binary. It is used only by tests and as a starting template.
package example

import (
	"context"
	"encoding/json"

	"github.com/denta-codex/codex-gateway/adapter"
)

type Adapter struct{}

func (Adapter) Namespace() string { return "example" }
func (Adapter) Models() []adapter.Model {
	return []adapter.Model{{Slug: "example/echo", Catalog: json.RawMessage(`{"slug":"example/echo","display_name":"Example Echo","description":"Adapter test model","visibility":"list","supported_in_api":true,"context_window":8192,"supported_reasoning_levels":[{"effort":"low","description":"Default"}],"default_reasoning_level":"low"}`)}}
}
func (Adapter) ServeResponses(_ context.Context, _ adapter.Request, sink adapter.EventSink) error {
	event, _ := json.Marshal(map[string]any{
		"type":     "response.completed",
		"response": map[string]any{"id": "example-response", "object": "response", "status": "completed", "model": "example/echo", "output": []any{}},
	})
	return sink.Emit(event)
}
