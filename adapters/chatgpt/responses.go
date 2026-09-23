package chatgpt

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func identifier(prefix string) string {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(id[:])
}

type toolEnvelope struct {
	Text      *string `json:"text"`
	ToolCalls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
}

func parseOutput(raw string, p prepared) ([]map[string]any, error) {
	if len(p.tools) == 0 {
		return textItems(raw), nil
	}
	var envelope toolEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, errors.New("ChatGPT returned invalid tool JSON")
	}
	if envelope.Text == nil || envelope.ToolCalls == nil || len(envelope.ToolCalls) > 64 || p.required && len(envelope.ToolCalls) == 0 || !p.parallel && len(envelope.ToolCalls) > 1 {
		return nil, errors.New("ChatGPT returned invalid tool calls")
	}
	items := textItems(*envelope.Text)
	for _, call := range envelope.ToolCalls {
		tool, ok := p.tools[call.Name]
		if !ok || len(call.Arguments) == 0 || len(call.Arguments) > 2<<20 {
			return nil, errors.New("ChatGPT returned an undeclared tool call")
		}
		var arguments map[string]json.RawMessage
		if json.Unmarshal(call.Arguments, &arguments) != nil || arguments == nil {
			return nil, errors.New("ChatGPT returned invalid tool arguments")
		}
		item := map[string]any{"id": identifier("fc_"), "call_id": identifier("call_"), "name": call.Name, "status": "completed"}
		if tool.Type == "custom" {
			var input string
			if json.Unmarshal(arguments["input"], &input) != nil {
				return nil, errors.New("ChatGPT returned invalid freeform tool input")
			}
			item["type"] = "custom_tool_call"
			item["input"] = input
		} else {
			item["type"] = "function_call"
			item["arguments"] = string(call.Arguments)
		}
		items = append(items, item)
	}
	return items, nil
}

func textItems(content string) []map[string]any {
	if content == "" {
		return nil
	}
	return []map[string]any{{"id": identifier("msg_"), "type": "message", "status": "completed", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": content, "annotations": []any{}}}}}
}

type streamWriter struct {
	w          http.ResponseWriter
	responseID string
	model      string
	started    bool
	textID     string
	seq        int
}

func (s *streamWriter) emit(value map[string]any) error {
	s.seq++
	value["sequence_number"] = s.seq
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	if flush, ok := s.w.(http.Flusher); ok {
		flush.Flush()
	}
	return nil
}

func (s *streamWriter) start() error {
	if s.started {
		return nil
	}
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.started = true
	return s.emit(map[string]any{"type": "response.created", "response": s.response("in_progress", nil)})
}

func (s *streamWriter) response(status string, output []map[string]any) map[string]any {
	if output == nil {
		output = []map[string]any{}
	}
	return map[string]any{"id": s.responseID, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": s.model, "output": output, "parallel_tool_calls": true}
}

func (s *streamWriter) textDelta(text string) error {
	if err := s.start(); err != nil {
		return err
	}
	if s.textID == "" {
		s.textID = identifier("msg_")
		item := map[string]any{"id": s.textID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}
		if err := s.emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}); err != nil {
			return err
		}
		if err := s.emit(map[string]any{"type": "response.content_part.added", "item_id": s.textID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	}
	return s.emit(map[string]any{"type": "response.output_text.delta", "item_id": s.textID, "output_index": 0, "content_index": 0, "delta": text})
}

func (s *streamWriter) finish(output []map[string]any) error {
	if err := s.start(); err != nil {
		return err
	}
	for index, item := range output {
		kind, _ := item["type"].(string)
		if kind == "message" && s.textID != "" {
			item["id"] = s.textID
			content := item["content"].([]map[string]any)[0]["text"]
			if err := s.emit(map[string]any{"type": "response.output_text.done", "item_id": s.textID, "output_index": index, "content_index": 0, "text": content}); err != nil {
				return err
			}
			if err := s.emit(map[string]any{"type": "response.content_part.done", "item_id": s.textID, "output_index": index, "content_index": 0, "part": item["content"].([]map[string]any)[0]}); err != nil {
				return err
			}
		} else {
			if err := s.emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item}); err != nil {
				return err
			}
		}
		if err := s.emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}); err != nil {
			return err
		}
	}
	return s.emit(map[string]any{"type": "response.completed", "response": s.response("completed", output)})
}

func (s *streamWriter) fail(message string) {
	if !s.started {
		return
	}
	_ = s.emit(map[string]any{"type": "response.failed", "response": map[string]any{"id": s.responseID, "object": "response", "status": "failed", "model": s.model, "output": []any{}, "error": map[string]string{"code": "chatgpt_chat_failed", "message": message}}})
}

func (a Adapter) serveResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	p, err := prepare(body)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_chatgpt_request", err.Error())
		return
	}
	writer := &streamWriter{w: w, responseID: identifier("resp_"), model: ModelSlug}
	var output strings.Builder
	onText := func(delta string) error {
		if output.Len()+len(delta) > maxPayload {
			return errors.New("ChatGPT output exceeds 8 MiB")
		}
		output.WriteString(delta)
		if p.stream && len(p.tools) == 0 {
			return writer.textDelta(delta)
		}
		return nil
	}
	err = runWithRefresh(r.Context(), a.Auth, a.Run, p, onText)
	if err != nil {
		status, message := publicTransportError(err)
		if writer.started {
			writer.fail(message)
		} else {
			respondError(w, status, "chatgpt_chat_failed", message)
		}
		return
	}
	items, err := parseOutput(output.String(), p)
	if err != nil {
		if writer.started {
			writer.fail("ChatGPT tool response was invalid")
		} else {
			respondError(w, 502, "chatgpt_tool_response_invalid", "ChatGPT tool response was invalid")
		}
		return
	}
	if p.stream {
		_ = writer.finish(items)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(writer.response("completed", items))
}

func respondError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": code, "message": message}})
}
