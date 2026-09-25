package modal

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeChatBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestTranslateRequestConversationAndTools(t *testing.T) {
	t.Parallel()
	body := `{
  "model":"modal/` + GLMFlashModel + `",
  "instructions":"be exact",
  "input":[
    {"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"}]},
    {"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"content":[]},
    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"calling"}]},
    {"type":"function_call","name":"read","call_id":"call_1","arguments":"{\"path\":\"x\"}"},
    {"type":"function_call_output","call_id":"call_1","output":"ok"}
	,{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"review","parameters":{"type":"object"}}]}
  ],
  "tools":[
    {"type":"function","name":"read","description":"read a file","parameters":{"type":"object"},"strict":true},
    {"type":"custom","name":"shell","description":"run shell"},
    {"type":"namespace","name":"browser","tools":[{"type":"function","name":"open","parameters":{"type":"object"}}]},
    {"type":"web_search_preview"}
  ],
  "tool_choice":"auto","parallel_tool_calls":true,"max_output_tokens":123,
  "reasoning":{"effort":"ultra"},"text":{"format":{"type":"json_object"}},"stream":true
}`
	translated, err := translateRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !translated.Stream || translated.Model != GLMFlashModel {
		t.Fatalf("unexpected translated metadata: %#v", translated)
	}
	chat := decodeChatBody(t, translated.Body)
	if chat["model"] != GLMFlashModel || chat["stream"] != true || chat["max_tokens"] != float64(123) || chat["reasoning_effort"] != "max" {
		t.Fatalf("unexpected Chat options: %#v", chat)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 4 {
		t.Fatalf("got %d translated tools, want 4", len(tools))
	}
	if _, ok := translated.Registry.byWire["browser__open"]; !ok {
		t.Fatal("namespaced tool was not registered")
	}
	messages := chat["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("got %d messages, want system, user, assistant, tool: %#v", len(messages), messages)
	}
	assistant := messages[2].(map[string]any)
	if assistant["reasoning_content"] != "thinking" || assistant["content"] == nil || len(assistant["tool_calls"].([]any)) != 1 {
		t.Fatalf("assistant history was not merged: %#v", assistant)
	}
}

func TestTranslateRequestDeduplicatesAdditionalTools(t *testing.T) {
	t.Parallel()
	payload := `{"model":"modal/` + GLMFlashModel + `","input":[{"type":"additional_tools","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},{"type":"message","role":"user","content":"hi"}],"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`
	translated, err := translateRequest([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if tools := decodeChatBody(t, translated.Body)["tools"].([]any); len(tools) != 1 {
		t.Fatalf("got %d copies of a repeated tool", len(tools))
	}
}

func TestTranslateRequestRejectsStateAndUnsupportedParts(t *testing.T) {
	t.Parallel()
	requests := map[string]string{
		"previous response": `{"model":"modal/` + GLMFlashModel + `","input":"hi","previous_response_id":"resp_1"}`,
		"audio":             `{"model":"modal/` + GLMFlashModel + `","input":[{"type":"message","role":"user","content":[{"type":"input_audio"}]}]}`,
	}
	for name, payload := range requests {
		t.Run(name, func(t *testing.T) {
			if _, err := translateRequest([]byte(payload)); err == nil {
				t.Fatalf("accepted unsupported request: %s", payload)
			}
		})
	}
}

func TestWireToolNameIsStableAndBounded(t *testing.T) {
	t.Parallel()
	name := wireToolName("namespace", strings.Repeat("x", 100))
	if len(name) > 64 || name != wireToolName("namespace", strings.Repeat("x", 100)) {
		t.Fatalf("invalid wire name %q", name)
	}
}

func TestTranslateAllowedToolsChoiceFiltersCatalog(t *testing.T) {
	t.Parallel()
	payload := `{"model":"modal/` + GLMFlashModel + `","input":"hi","tools":[{"type":"function","name":"read","parameters":{"type":"object"}},{"type":"custom","name":"shell"}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"custom","name":"shell"}]}}`
	translated, err := translateRequest([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	chat := decodeChatBody(t, translated.Body)
	tools := chat["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "shell" {
		t.Fatalf("allowed tool catalog was not narrowed: %#v", tools)
	}
	choice := chat["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "shell" {
		t.Fatalf("single required tool was not forced: %#v", choice)
	}
}
