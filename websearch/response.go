package websearch

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type bufferedRound struct {
	status int
	header http.Header
	body   []byte
	output []map[string]any
}

func readRound(response *http.Response, limit int64) (*bufferedRound, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("empty backend response")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("backend response exceeded limit")
	}
	round := &bufferedRound{status: response.StatusCode, header: response.Header.Clone(), body: body}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return round, nil
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") || looksLikeSSE(body) {
		round.output, _, _, _, err = parseSSE(body)
	} else {
		round.output, _, err = parseJSONResponse(body)
	}
	if err != nil {
		return nil, err
	}
	return round, nil
}

func looksLikeSSE(body []byte) bool {
	prefix := bytes.TrimSpace(body)
	if len(prefix) > 4096 {
		prefix = prefix[:4096]
	}
	return bytes.HasPrefix(prefix, []byte("data:")) || bytes.HasPrefix(prefix, []byte("event:")) || bytes.Contains(prefix, []byte("\ndata:"))
}

func parseJSONResponse(body []byte) ([]map[string]any, string, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", err
	}
	output := mapsFrom(payload["output"])
	return output, textFromOutput(output), nil
}

func parseSSE(body []byte) ([]map[string]any, string, []source, bool, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64<<10), 2<<20)
	var dataLines []string
	var doneOutput []map[string]any
	var completedOutput []map[string]any
	var deltas strings.Builder
	var sources []source
	failed := false
	consume := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			return nil
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		typeName, _ := event["type"].(string)
		switch typeName {
		case "response.output_text.delta":
			if delta, ok := event["delta"].(string); ok {
				deltas.WriteString(delta)
			}
		case "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok {
				doneOutput = append(doneOutput, item)
			}
		case "response.completed", "response.incomplete":
			if response, ok := event["response"].(map[string]any); ok {
				completedOutput = mapsFrom(response["output"])
			}
		case "response.failed", "error":
			failed = true
		}
		collectSources(event, &sources)
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := consume(); err != nil {
				return nil, "", nil, false, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, "", nil, false, err
	}
	if err := consume(); err != nil {
		return nil, "", nil, false, err
	}
	output := completedOutput
	if len(output) == 0 {
		output = doneOutput
	}
	text := textFromOutput(output)
	if text == "" {
		text = deltas.String()
	}
	collectOutputSources(output, &sources)
	return output, text, dedupeSources(sources), failed, nil
}

func mapsFrom(raw any) []map[string]any {
	items, _ := raw.([]any)
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if value, ok := item.(map[string]any); ok {
			result = append(result, value)
		}
	}
	return result
}

func textFromOutput(output []map[string]any) string {
	var text strings.Builder
	for _, item := range output {
		if kind, _ := item["type"].(string); kind != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, raw := range content {
			part, _ := raw.(map[string]any)
			kind, _ := part["type"].(string)
			if kind == "output_text" || kind == "text" {
				value, _ := part["text"].(string)
				text.WriteString(value)
			}
		}
	}
	return text.String()
}

var hopHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true,
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		lower := strings.ToLower(key)
		if hopHeaders[lower] || lower == "content-length" || lower == "set-cookie" {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func relayResponse(w http.ResponseWriter, response *http.Response) {
	if response == nil || response.Body == nil {
		writeError(w, http.StatusBadGateway, "routed_model_invalid_response", "routed model returned an empty response")
		return
	}
	defer response.Body.Close()
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (r *bufferedRound) relay(w http.ResponseWriter) {
	copyHeaders(w.Header(), r.header)
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body)
}

func responseID() string {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("resp_%x", time.Now().UnixNano())
	}
	return "resp_" + hex.EncodeToString(raw[:])
}

func writeOutput(w http.ResponseWriter, stream bool, model string, output []map[string]any) {
	id := responseID()
	response := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": model, "output": output,
		"parallel_tool_calls": true,
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sequence := 0
	emit := func(value map[string]any) {
		sequence++
		value["sequence_number"] = sequence
		encoded, _ := json.Marshal(value)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", encoded)
	}
	created := map[string]any{}
	for key, value := range response {
		created[key] = value
	}
	created["status"] = "in_progress"
	created["output"] = []any{}
	emit(map[string]any{"type": "response.created", "response": created})
	for index, item := range output {
		emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
		emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	emit(map[string]any{"type": "response.completed", "response": response})
}
