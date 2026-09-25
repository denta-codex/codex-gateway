package modal

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func eventTypes(t *testing.T, body string) []string {
	t.Helper()
	var types []string
	for _, frame := range strings.Split(body, "\n\n") {
		line := strings.TrimSpace(frame)
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			t.Fatalf("invalid downstream event: %v\n%s", err, line)
		}
		types = append(types, event["type"].(string))
	}
	return types
}

func hasEvent(events []string, wanted string) bool {
	for _, event := range events {
		if event == wanted {
			return true
		}
	}
	return false
}

func TestBridgeChatStreamReasoningSummaryAndUsage(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	translated := translatedRequest{Model: GLMFlashModel, Stream: true, Registry: newToolRegistry()}
	bridge := newResponseBridge(recorder, translated)
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	if err := bridgeChatStream(bytes.NewBufferString(sse), bridge); err != nil {
		t.Fatal(err)
	}
	events := eventTypes(t, recorder.Body.String())
	for _, wanted := range []string{
		"response.created",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_text.delta",
		"response.completed",
	} {
		if !hasEvent(events, wanted) {
			t.Fatalf("missing %s in %v", wanted, events)
		}
	}
	if hasEvent(events, "response.reasoning_text.delta") || hasEvent(events, "response.reasoning_text.done") {
		t.Fatalf("raw reasoning events leaked into Responses output: %v", events)
	}
	var completed struct {
		Type     string `json:"type"`
		Response struct {
			Output []struct {
				Type    string            `json:"type"`
				Summary []map[string]any  `json:"summary"`
				Content []json.RawMessage `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
		line := strings.TrimSpace(frame)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var candidate struct {
			Type string `json:"type"`
		}
		payload := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		if json.Unmarshal(payload, &candidate) == nil && candidate.Type == "response.completed" {
			if err := json.Unmarshal(payload, &completed); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if len(completed.Response.Output) < 1 {
		t.Fatal("completed response did not include reasoning output")
	}
	reasoning := completed.Response.Output[0]
	if reasoning.Type != "reasoning" || len(reasoning.Content) != 0 || len(reasoning.Summary) != 1 ||
		reasoning.Summary[0]["type"] != "summary_text" || reasoning.Summary[0]["text"] != "think" {
		t.Fatalf("reasoning was not stored as a summary: %#v", reasoning)
	}
	if strings.Index(recorder.Body.String(), `"type":"reasoning"`) > strings.LastIndex(recorder.Body.String(), `"type":"message"`) {
		t.Fatal("final output was not kept in output_index order")
	}
}

func TestBridgeChatStreamParallelFunctionAndCustomCalls(t *testing.T) {
	t.Parallel()
	registry := newToolRegistry()
	_ = registry.add(toolIdentity{Name: "read", WireName: "read", Kind: functionTool})
	_ = registry.add(toolIdentity{Name: "shell", WireName: "shell", Kind: customTool})
	recorder := httptest.NewRecorder()
	bridge := newResponseBridge(recorder, translatedRequest{Model: GLMFlashModel, Stream: true, Registry: registry})
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read","arguments":"{\"pa"}},{"index":1,"id":"call_b","function":{"name":"shell","arguments":"{\"in"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"x\"}"}},{"index":1,"function":{"arguments":"put\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	if err := bridgeChatStream(bytes.NewBufferString(sse), bridge); err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	events := eventTypes(t, out)
	for _, wanted := range []string{"response.function_call_arguments.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.completed"} {
		if !hasEvent(events, wanted) {
			t.Fatalf("missing %s in %s", wanted, out)
		}
	}
}

func TestBridgeChatStreamRejectsTruncation(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	bridge := newResponseBridge(recorder, translatedRequest{Model: GLMFlashModel, Stream: true, Registry: newToolRegistry()})
	err := bridgeChatStream(bytes.NewBufferString(`data: {"choices":[{"delta":{"content":"partial"}}]}`+"\n\n"), bridge)
	if err == nil || !strings.Contains(err.Error(), "ended without") {
		t.Fatalf("unexpected error: %v", err)
	}
	bridge.fail(err.Error())
	events := eventTypes(t, recorder.Body.String())
	if !hasEvent(events, "response.failed") || hasEvent(events, "response.completed") {
		t.Fatalf("truncated stream did not fail closed: %v", events)
	}
}

func TestBridgeRejectsUndeclaredAndMalformedToolCalls(t *testing.T) {
	t.Parallel()
	for name, sse := range map[string]string{
		"undeclared": `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"missing","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n",
		"malformed":  `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			registry := newToolRegistry()
			_ = registry.add(toolIdentity{Name: "read", WireName: "read", Kind: functionTool})
			bridge := newResponseBridge(httptest.NewRecorder(), translatedRequest{Model: GLMFlashModel, Stream: true, Registry: registry})
			if err := bridgeChatStream(bytes.NewBufferString(sse), bridge); err == nil {
				t.Fatal("accepted invalid tool stream")
			}
		})
	}
}

func TestBridgeDoesNotEchoUpstreamErrorPayload(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	bridge := newResponseBridge(recorder, translatedRequest{Model: GLMFlashModel, Stream: true, Registry: newToolRegistry()})
	err := bridgeChatStream(bytes.NewBufferString(`data: {"error":{"message":"sensitive-upstream-detail"}}`+"\n\n"), bridge)
	if err == nil {
		t.Fatal("accepted upstream error event")
	}
	bridge.fail(err.Error())
	if strings.Contains(recorder.Body.String(), "sensitive-upstream-detail") || !hasEvent(eventTypes(t, recorder.Body.String()), "response.failed") {
		t.Fatalf("upstream error was exposed or not failed: %s", recorder.Body.String())
	}
}

func TestBridgeRejectsToolOutsideChoice(t *testing.T) {
	t.Parallel()
	registry := newToolRegistry()
	_ = registry.add(toolIdentity{Name: "read", WireName: "read", Kind: functionTool})
	_ = registry.add(toolIdentity{Name: "write", WireName: "write", Kind: functionTool})
	registry.permitOnly("read")
	bridge := newResponseBridge(httptest.NewRecorder(), translatedRequest{Model: GLMFlashModel, Stream: true, Registry: registry})
	sse := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"write","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	if err := bridgeChatStream(bytes.NewBufferString(sse), bridge); err == nil || !strings.Contains(err.Error(), "outside tool_choice") {
		t.Fatalf("unexpected error: %v", err)
	}
}
