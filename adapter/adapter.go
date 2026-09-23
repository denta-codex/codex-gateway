// Package adapter defines the compile-time extension point for additional models.
package adapter

import (
	"context"
	"encoding/json"
	"net/http"
)

// Model carries a Codex catalog row. Its slug must start with the adapter
// namespace followed by a slash. TemplateNative fills schema fields from a
// current native row before applying Catalog as top-level overrides. Adapters
// using it must explicitly override capabilities they do not implement.
type Model struct {
	Slug           string
	Catalog        json.RawMessage
	TemplateNative bool
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

// TokenSource exposes Grace's managed ChatGPT login to an explicitly linked
// adapter that uses another ChatGPT endpoint. Implementations must never send
// this token to a non-ChatGPT host.
type TokenSource interface {
	Token(context.Context) (string, string, error)
}

// SubscriptionAuthConsumer is optional. The app binds the managed login before
// catalog generation or request serving.
type SubscriptionAuthConsumer interface {
	SetSubscriptionAuth(TokenSource)
}
