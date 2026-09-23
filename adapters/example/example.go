// Package example shows the adapter contract without being registered in the
// production binary. It is used only by tests and as a starting template.
package example

import (
	"encoding/json"
	"net/http"

	"github.com/denta-codex/codex-gateway/adapter"
)

type Adapter struct{}

func (Adapter) Namespace() string { return "example" }
func (Adapter) Models() []adapter.Model {
	return []adapter.Model{{Slug: "example/echo", Catalog: json.RawMessage(`{"slug":"example/echo","display_name":"Example Echo","description":"Adapter test model","visibility":"list","supported_in_api":true,"context_window":8192,"supported_reasoning_levels":[{"effort":"low","description":"Default"}],"default_reasoning_level":"low"}`)}}
}
func (Adapter) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "example-response", "object": "response", "model": "example/echo", "output": []any{}})
}
