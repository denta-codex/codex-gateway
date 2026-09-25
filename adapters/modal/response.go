package modal

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const maxSSEFrame = 8 << 20

func identifier(prefix string) string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%s%x", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(value[:])
}

type openText struct {
	id    string
	index int
	text  strings.Builder
}

type openReasoning struct {
	id    string
	index int
	text  strings.Builder
}

type openToolCall struct {
	index       int
	itemIndex   int
	id          string
	wireName    string
	arguments   strings.Builder
	identity    toolIdentity
	itemStarted bool
}

type responseBridge struct {
	w          http.ResponseWriter
	stream     bool
	model      string
	registry   *toolRegistry
	responseID string
	createdAt  int64
	sequence   int
	started    bool
	terminal   bool
	nextIndex  int
	output     map[int]map[string]any
	usage      map[string]any
	finish     string
	text       *openText
	reasoning  *openReasoning
	calls      map[int]*openToolCall
	callOrder  []int
}

func newResponseBridge(w http.ResponseWriter, translated translatedRequest) *responseBridge {
	return &responseBridge{
		w: w, stream: translated.Stream, model: Namespace + "/" + translated.Model,
		registry: translated.Registry, responseID: identifier("resp_"), createdAt: time.Now().Unix(),
		calls: map[int]*openToolCall{}, output: map[int]map[string]any{},
	}
}

func (b *responseBridge) snapshot(status string) map[string]any {
	indexes := make([]int, 0, len(b.output))
	for index := range b.output {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	output := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		output = append(output, b.output[index])
	}
	response := map[string]any{
		"id": b.responseID, "object": "response", "created_at": b.createdAt,
		"status": status, "model": b.model, "output": output, "parallel_tool_calls": true,
	}
	if b.usage != nil {
		response["usage"] = b.usage
	}
	return response
}

func (b *responseBridge) start() error {
	if b.started || !b.stream {
		return nil
	}
	b.w.Header().Set("Content-Type", "text/event-stream")
	b.w.Header().Set("Cache-Control", "no-cache")
	b.w.Header().Set("X-Accel-Buffering", "no")
	b.w.WriteHeader(http.StatusOK)
	b.started = true
	return b.emit("response.created", map[string]any{"response": b.snapshot("in_progress")})
}

func (b *responseBridge) emit(kind string, fields map[string]any) error {
	if !b.stream {
		return nil
	}
	if !b.started {
		if err := b.start(); err != nil {
			return err
		}
	}
	b.sequence++
	fields["type"] = kind
	fields["sequence_number"] = b.sequence
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(b.w, "data: %s\n\n", data); err != nil {
		return err
	}
	if flush, ok := b.w.(http.Flusher); ok {
		flush.Flush()
	}
	return nil
}

func (b *responseBridge) reasoningDelta(text string) error {
	if text == "" {
		return nil
	}
	if b.text != nil {
		if err := b.closeText("commentary"); err != nil {
			return err
		}
	}
	if b.reasoning == nil {
		b.reasoning = &openReasoning{id: identifier("rs_"), index: b.nextIndex}
		b.nextIndex++
		item := map[string]any{"type": "reasoning", "id": b.reasoning.id, "summary": []any{}, "content": []any{}}
		if err := b.emit("response.output_item.added", map[string]any{"output_index": b.reasoning.index, "item": item}); err != nil {
			return err
		}
		part := map[string]any{"type": "summary_text", "text": ""}
		if err := b.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": b.reasoning.id, "output_index": b.reasoning.index, "summary_index": 0, "part": part,
		}); err != nil {
			return err
		}
	}
	b.reasoning.text.WriteString(text)
	return b.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": b.reasoning.id, "output_index": b.reasoning.index, "summary_index": 0, "delta": text,
	})
}

func (b *responseBridge) textDelta(text string) error {
	if text == "" {
		return nil
	}
	if b.reasoning != nil {
		if err := b.closeReasoning(); err != nil {
			return err
		}
	}
	if b.text == nil {
		b.text = &openText{id: identifier("msg_"), index: b.nextIndex}
		b.nextIndex++
		item := map[string]any{"type": "message", "id": b.text.id, "status": "in_progress", "role": "assistant", "content": []any{}}
		if err := b.emit("response.output_item.added", map[string]any{"output_index": b.text.index, "item": item}); err != nil {
			return err
		}
		part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
		if err := b.emit("response.content_part.added", map[string]any{"item_id": b.text.id, "output_index": b.text.index, "content_index": 0, "part": part}); err != nil {
			return err
		}
	}
	b.text.text.WriteString(text)
	return b.emit("response.output_text.delta", map[string]any{
		"item_id": b.text.id, "output_index": b.text.index, "content_index": 0, "delta": text,
	})
}

func (b *responseBridge) closeText(phase string) error {
	if b.text == nil {
		return nil
	}
	text := b.text.text.String()
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	if err := b.emit("response.output_text.done", map[string]any{"item_id": b.text.id, "output_index": b.text.index, "content_index": 0, "text": text}); err != nil {
		return err
	}
	if err := b.emit("response.content_part.done", map[string]any{"item_id": b.text.id, "output_index": b.text.index, "content_index": 0, "part": part}); err != nil {
		return err
	}
	item := map[string]any{"type": "message", "id": b.text.id, "status": "completed", "role": "assistant", "content": []any{part}}
	if phase != "" {
		item["phase"] = phase
	}
	if err := b.emit("response.output_item.done", map[string]any{"output_index": b.text.index, "item": item}); err != nil {
		return err
	}
	b.output[b.text.index] = item
	b.text = nil
	return nil
}

func (b *responseBridge) closeReasoning() error {
	if b.reasoning == nil {
		return nil
	}
	text := b.reasoning.text.String()
	if err := b.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": b.reasoning.id, "output_index": b.reasoning.index, "summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	part := map[string]any{"type": "summary_text", "text": text}
	if err := b.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id": b.reasoning.id, "output_index": b.reasoning.index, "summary_index": 0, "part": part,
	}); err != nil {
		return err
	}
	item := map[string]any{
		"type": "reasoning", "id": b.reasoning.id, "summary": []any{part}, "content": []any{},
	}
	if err := b.emit("response.output_item.done", map[string]any{"output_index": b.reasoning.index, "item": item}); err != nil {
		return err
	}
	b.output[b.reasoning.index] = item
	b.reasoning = nil
	return nil
}

func (b *responseBridge) toolDelta(index int, id, name, arguments string) error {
	if b.text != nil {
		if err := b.closeText("commentary"); err != nil {
			return err
		}
	}
	if b.reasoning != nil {
		if err := b.closeReasoning(); err != nil {
			return err
		}
	}
	call := b.calls[index]
	if call == nil {
		call = &openToolCall{index: index, itemIndex: -1}
		b.calls[index] = call
		b.callOrder = append(b.callOrder, index)
	}
	if id != "" && call.id == "" {
		call.id = id
	}
	if name != "" && call.wireName == "" {
		call.wireName = name
	}
	if arguments != "" {
		if call.arguments.Len()+len(arguments) > maxSSEFrame {
			return errors.New("upstream tool arguments exceed adapter limit")
		}
		call.arguments.WriteString(arguments)
	}
	startedNow := false
	if !call.itemStarted && call.wireName != "" {
		identity, ok := b.registry.byWire[call.wireName]
		if !ok {
			return fmt.Errorf("upstream emitted undeclared tool %q", call.wireName)
		}
		if !b.registry.permits(call.wireName) {
			return fmt.Errorf("upstream emitted tool %q outside tool_choice", call.wireName)
		}
		call.identity = identity
		if call.id == "" {
			call.id = identifier("call_")
		}
		call.itemIndex = b.nextIndex
		b.nextIndex++
		call.itemStarted = true
		startedNow = true
		itemID := identifier(map[toolKind]string{functionTool: "fc_", customTool: "ctc_"}[identity.Kind])
		if identity.Kind == customTool {
			item := map[string]any{"type": "custom_tool_call", "id": itemID, "call_id": call.id, "name": identity.Name, "input": "", "status": "in_progress"}
			if identity.Namespace != "" {
				item["namespace"] = identity.Namespace
			}
			call.wireName = itemID
			return b.emit("response.output_item.added", map[string]any{"output_index": call.itemIndex, "item": item})
		}
		item := map[string]any{"type": "function_call", "id": itemID, "call_id": call.id, "name": identity.Name, "arguments": "", "status": "in_progress"}
		if identity.Namespace != "" {
			item["namespace"] = identity.Namespace
		}
		call.wireName = itemID
		if err := b.emit("response.output_item.added", map[string]any{"output_index": call.itemIndex, "item": item}); err != nil {
			return err
		}
	}
	if call.itemStarted && call.identity.Kind == functionTool && (arguments != "" || startedNow && call.arguments.Len() > 0) {
		delta := arguments
		if startedNow {
			delta = call.arguments.String()
		}
		return b.emit("response.function_call_arguments.delta", map[string]any{"item_id": call.wireName, "output_index": call.itemIndex, "delta": delta})
	}
	return nil
}

func (b *responseBridge) closeToolCalls(incomplete bool) error {
	for _, index := range b.callOrder {
		call := b.calls[index]
		if call == nil {
			continue
		}
		if !call.itemStarted {
			if incomplete {
				continue
			}
			return errors.New("upstream tool call did not include a declared name")
		}
		arguments := call.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		status := "completed"
		if incomplete {
			status = "incomplete"
		}
		var item map[string]any
		if call.identity.Kind == customTool {
			input := ""
			var wrapper struct {
				Input *string `json:"input"`
			}
			if !incomplete && (json.Unmarshal([]byte(arguments), &wrapper) != nil || wrapper.Input == nil) {
				return fmt.Errorf("upstream emitted malformed custom tool input for %q", call.identity.Name)
			}
			if wrapper.Input != nil {
				input = *wrapper.Input
			}
			if !incomplete {
				if err := b.emit("response.custom_tool_call_input.delta", map[string]any{"item_id": call.wireName, "output_index": call.itemIndex, "delta": input}); err != nil {
					return err
				}
				if err := b.emit("response.custom_tool_call_input.done", map[string]any{"item_id": call.wireName, "output_index": call.itemIndex, "input": input}); err != nil {
					return err
				}
			}
			item = map[string]any{"type": "custom_tool_call", "id": call.wireName, "call_id": call.id, "name": call.identity.Name, "input": input, "status": status}
		} else {
			var object map[string]any
			if !incomplete && (json.Unmarshal([]byte(arguments), &object) != nil || object == nil) {
				return fmt.Errorf("upstream emitted malformed function arguments for %q", call.identity.Name)
			}
			if !incomplete {
				if err := b.emit("response.function_call_arguments.done", map[string]any{"item_id": call.wireName, "output_index": call.itemIndex, "arguments": arguments}); err != nil {
					return err
				}
			}
			item = map[string]any{"type": "function_call", "id": call.wireName, "call_id": call.id, "name": call.identity.Name, "arguments": arguments, "status": status}
		}
		if call.identity.Namespace != "" {
			item["namespace"] = call.identity.Namespace
		}
		if err := b.emit("response.output_item.done", map[string]any{"output_index": call.itemIndex, "item": item}); err != nil {
			return err
		}
		b.output[call.itemIndex] = item
	}
	b.calls = map[int]*openToolCall{}
	b.callOrder = nil
	return nil
}

func (b *responseBridge) consumeChunk(payload map[string]any) error {
	if rawUsage, ok := payload["usage"].(map[string]any); ok {
		b.usage = responsesUsage(rawUsage)
	}
	if upstreamError := payload["error"]; upstreamError != nil {
		return errors.New("Modal Chat stream reported an error")
	}
	rawChoices, exists := payload["choices"]
	if !exists {
		return nil
	}
	choices, ok := rawChoices.([]any)
	if !ok {
		return errors.New("upstream choices is not an array")
	}
	if len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return errors.New("upstream choice is not an object")
	}
	if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
		b.finish = finish
	}
	delta, _ := choice["delta"].(map[string]any)
	if delta == nil {
		return nil
	}
	if reasoning := reasoningText(delta); reasoning != "" {
		if err := b.reasoningDelta(reasoning); err != nil {
			return err
		}
	}
	if content, ok := delta["content"].(string); ok && content != "" {
		if err := b.textDelta(content); err != nil {
			return err
		}
	}
	if rawCalls := delta["tool_calls"]; rawCalls != nil {
		calls, ok := rawCalls.([]any)
		if !ok {
			return errors.New("upstream tool_calls is not an array")
		}
		for position, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				return errors.New("upstream tool call is not an object")
			}
			index := position
			if value, ok := call["index"].(float64); ok {
				if value < 0 || value != float64(int(value)) {
					return errors.New("upstream tool call index is invalid")
				}
				index = int(value)
			}
			id, _ := call["id"].(string)
			function, _ := call["function"].(map[string]any)
			name, arguments := "", ""
			if function != nil {
				name, _ = function["name"].(string)
				arguments, _ = function["arguments"].(string)
			}
			if err := b.toolDelta(index, id, name, arguments); err != nil {
				return err
			}
		}
	}
	return nil
}

func reasoningText(delta map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if value, ok := delta[key].(string); ok {
			return value
		}
	}
	return ""
}

func responsesUsage(input map[string]any) map[string]any {
	integer := func(key string) int64 {
		switch value := input[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		case int:
			return int64(value)
		}
		return 0
	}
	in, out := integer("prompt_tokens"), integer("completion_tokens")
	usage := map[string]any{
		"input_tokens": in, "output_tokens": out, "total_tokens": integer("total_tokens"),
		"input_tokens_details":  map[string]any{"cached_tokens": int64(0)},
		"output_tokens_details": map[string]any{"reasoning_tokens": int64(0)},
	}
	if details, ok := input["prompt_tokens_details"].(map[string]any); ok {
		if cached, ok := details["cached_tokens"].(float64); ok {
			usage["input_tokens_details"] = map[string]any{"cached_tokens": int64(cached)}
		}
	}
	if details, ok := input["completion_tokens_details"].(map[string]any); ok {
		if reasoning, ok := details["reasoning_tokens"].(float64); ok {
			usage["output_tokens_details"] = map[string]any{"reasoning_tokens": int64(reasoning)}
		}
	}
	return usage
}

func (b *responseBridge) complete() error {
	if b.terminal {
		return nil
	}
	if err := b.closeReasoning(); err != nil {
		return err
	}
	phase := "final_answer"
	if len(b.calls) > 0 {
		phase = "commentary"
	}
	if err := b.closeText(phase); err != nil {
		return err
	}
	if err := b.closeToolCalls(false); err != nil {
		return err
	}
	if b.finish == "length" || b.finish == "max_tokens" || b.finish == "model_context_window_exceeded" || b.finish == "content_filter" || b.finish == "content-filter" || b.finish == "refusal" {
		b.terminal = true
		response := b.snapshot("incomplete")
		reason := "max_output_tokens"
		if b.finish == "content_filter" || b.finish == "content-filter" || b.finish == "refusal" {
			reason = "content_filter"
		}
		response["incomplete_details"] = map[string]any{"reason": reason}
		if b.stream {
			return b.emit("response.incomplete", map[string]any{"response": response})
		}
		return writeJSON(b.w, http.StatusOK, response)
	}
	if b.finish != "" && b.finish != "stop" && b.finish != "tool_calls" && b.finish != "function_call" {
		return fmt.Errorf("Modal Chat returned unsupported finish reason %q", b.finish)
	}
	b.terminal = true
	response := b.snapshot("completed")
	if b.stream {
		return b.emit("response.completed", map[string]any{"response": response})
	}
	return writeJSON(b.w, http.StatusOK, response)
}

func (b *responseBridge) fail(message string) {
	if b.terminal {
		return
	}
	_ = b.closeText("commentary")
	_ = b.closeReasoning()
	_ = b.closeToolCalls(true)
	b.terminal = true
	errorBody := map[string]any{"code": "modal_chat_failed", "message": message, "type": "upstream_error"}
	response := b.snapshot("failed")
	response["error"] = errorBody
	response["last_error"] = errorBody
	if b.stream {
		_ = b.emit("response.failed", map[string]any{"response": response})
		return
	}
	_ = writeJSON(b.w, http.StatusBadGateway, map[string]any{"error": errorBody})
}

func bridgeChatStream(body io.Reader, bridge *responseBridge) error {
	if bridge.stream {
		if err := bridge.start(); err != nil {
			return err
		}
	}
	reader := bufio.NewReaderSize(body, 64<<10)
	var data strings.Builder
	dispatch := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			return io.EOF
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			return errors.New("Modal returned malformed Chat SSE")
		}
		return bridge.consumeChunk(chunk)
	}
	for {
		line, err := reader.ReadString('\n')
		if data.Len()+len(line) > maxSSEFrame {
			return errors.New("Modal SSE frame exceeds adapter limit")
		}
		trimmed := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if trimmed == "" {
			if dispatchErr := dispatch(); errors.Is(dispatchErr, io.EOF) {
				return bridge.complete()
			} else if dispatchErr != nil {
				return dispatchErr
			}
		} else if strings.HasPrefix(trimmed, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			if dispatchErr := dispatch(); errors.Is(dispatchErr, io.EOF) {
				return bridge.complete()
			} else if dispatchErr != nil {
				return dispatchErr
			}
			if bridge.finish != "" {
				return bridge.complete()
			}
			return errors.New("Modal Chat stream ended without finish_reason or [DONE]")
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(value)
}
