package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/denta-codex/codex-gateway/adapter"
)

var adapterMetadataHeaders = []string{
	"Conversation-Id",
	"Originator",
	"Session-Id",
	"Thread-Id",
	"User-Agent",
	"X-Codex-Parent-Thread-Id",
}

func adapterRequest(body []byte, header http.Header) adapter.Request {
	metadata := make(map[string][]string, len(adapterMetadataHeaders))
	for _, key := range adapterMetadataHeaders {
		if values := header.Values(key); len(values) > 0 {
			metadata[key] = append([]string(nil), values...)
		}
	}
	return adapter.Request{Body: append(json.RawMessage(nil), body...), Metadata: metadata}
}

func requestStream(body []byte) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

func previousResponseID(body []byte) string {
	var request struct {
		PreviousResponseID string `json:"previous_response_id"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	return request.PreviousResponseID
}

type adapterHTTPEvents struct {
	w       http.ResponseWriter
	stream  bool
	started bool
	final   json.RawMessage
}

func (s *adapterHTTPEvents) Emit(event json.RawMessage) error {
	if !json.Valid(event) {
		return errors.New("adapter emitted invalid JSON")
	}
	if s.stream {
		if !s.started {
			s.w.Header().Set("Content-Type", "text/event-stream")
			s.w.Header().Set("Cache-Control", "no-cache")
			s.w.Header().Set("X-Accel-Buffering", "no")
			s.w.WriteHeader(http.StatusOK)
			s.started = true
		}
		if _, err := fmt.Fprintf(s.w, "data: %s\n\n", event); err != nil {
			return err
		}
		if flusher, ok := s.w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	var envelope struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(event, &envelope) != nil {
		return errors.New("adapter emitted an invalid Responses event")
	}
	switch envelope.Type {
	case "response.completed", "response.incomplete", "response.failed":
		if len(envelope.Response) == 0 || string(envelope.Response) == "null" {
			return errors.New("adapter terminal event omitted response")
		}
		s.final = append(s.final[:0], envelope.Response...)
	}
	return nil
}

func (g *Gateway) serveAdapterHTTP(w http.ResponseWriter, r *http.Request, extension adapter.Adapter, body []byte) {
	if previousResponseID(body) != "" {
		writeAdapterError(w, adapter.NewError(http.StatusNotFound, "previous_response_not_found", "Previous response not found. Retry with self-contained input.", nil))
		return
	}
	sink := &adapterHTTPEvents{w: w, stream: requestStream(body)}
	err := extension.ServeResponses(r.Context(), adapterRequest(body, r.Header), sink)
	if err != nil {
		if sink.started {
			_ = sink.Emit(adapterErrorEvent(err, ""))
			return
		}
		writeAdapterError(w, publicAdapterError(err))
		return
	}
	if sink.stream {
		return
	}
	if len(sink.final) == 0 {
		writeAdapterError(w, adapter.NewError(http.StatusBadGateway, "adapter_invalid_response", "Adapter did not emit a terminal response.", nil))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(sink.final, '\n'))
}

func publicAdapterError(err error) *adapter.Error {
	var public *adapter.Error
	if errors.As(err, &public) {
		result := *public
		if result.Status == 0 {
			result.Status = http.StatusBadGateway
		}
		if result.Code == "" {
			result.Code = "adapter_error"
		}
		if result.Message == "" {
			result.Message = "Adapter request failed."
		}
		return &result
	}
	return adapter.NewError(http.StatusBadGateway, "adapter_error", "Adapter request failed.", err)
}

func writeAdapterError(w http.ResponseWriter, err *adapter.Error) {
	errorBody := adapterErrorBody(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(err.Status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errorBody})
}

func adapterErrorEvent(err error, streamID string) json.RawMessage {
	public := publicAdapterError(err)
	event := map[string]any{
		"type":  "error",
		"error": adapterErrorBody(public),
	}
	if streamID != "" {
		event["stream_id"] = streamID
	}
	data, _ := json.Marshal(event)
	return data
}

func adapterErrorBody(err *adapter.Error) map[string]any {
	errorType := "adapter_error"
	result := map[string]any{"type": errorType, "code": err.Code, "message": err.Message}
	if err.Code == "previous_response_not_found" {
		result["type"] = "invalid_request_error"
		result["param"] = "previous_response_id"
	}
	return result
}
