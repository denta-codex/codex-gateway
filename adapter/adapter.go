// Package adapter defines the compile-time extension point for additional models.
package adapter

import (
	"encoding/json"
	"net/http"
)

// Model carries a complete Codex catalog row. Its slug must start with the
// adapter namespace followed by a slash. Keeping the row opaque lets an
// adapter own its provider-specific capabilities without changing the router.
type Model struct {
	Slug    string
	Catalog json.RawMessage
}

// Adapter is linked into a gateway binary. There is no runtime plugin loader.
// ServeResponses receives the original JSON body and must produce a Responses
// HTTP result, including streaming when requested. It owns all translation and
// upstream credentials for its namespace.
type Adapter interface {
	Namespace() string
	Models() []Model
	ServeResponses(http.ResponseWriter, *http.Request, []byte)
}
