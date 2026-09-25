// Package adapter defines the compile-time extension point for additional models.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
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

// Request is the transport-neutral input to an adapter. Metadata contains only
// caller metadata selected by the gateway; credentials are never included.
type Request struct {
	Body     json.RawMessage
	Metadata map[string][]string
}

// MetadataValue returns the first value for name, using HTTP-style
// case-insensitive matching without coupling adapters to an HTTP request.
func (r Request) MetadataValue(name string) string {
	for key, values := range r.Metadata {
		if len(values) > 0 && equalFoldASCII(key, name) {
			return values[0]
		}
	}
	return ""
}

func equalFoldASCII(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		a, b := left[index], right[index]
		if a >= 'A' && a <= 'Z' {
			a += 'a' - 'A'
		}
		if b >= 'A' && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// EventSink receives canonical OpenAI Responses events. The gateway decides
// whether to encode them as HTTP SSE, a final JSON response, or WebSocket
// messages. Adapters remain free to use any upstream transport.
type EventSink interface {
	Emit(json.RawMessage) error
}

type EventSinkFunc func(json.RawMessage) error

func (f EventSinkFunc) Emit(event json.RawMessage) error { return f(event) }

// Error is safe to expose to a Codex client. Cause is retained for logs and
// errors.Is/As but is never serialized by the gateway.
type Error struct {
	Status  int
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func NewError(status int, code, message string, cause error) *Error {
	return &Error{Status: status, Code: code, Message: message, Cause: cause}
}

// Adapter is linked into a gateway binary. There is no runtime plugin loader.
// ServeResponses emits canonical Responses events and owns all translation,
// upstream transport, and credentials for its namespace.
type Adapter interface {
	Namespace() string
	Models() []Model
	ServeResponses(context.Context, Request, EventSink) error
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
