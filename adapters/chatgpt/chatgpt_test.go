package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	gatewayadapter "github.com/denta-codex/codex-gateway/adapter"
)

func serveAdapterForTest(t *testing.T, implementation Adapter, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	err := implementation.ServeResponses(context.Background(), gatewayadapter.Request{Body: body}, gatewayadapter.EventSinkFunc(func(event json.RawMessage) error {
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := fmt.Fprintf(w, "data: %s\n\n", event)
			return err
		}
		var terminal struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(event, &terminal) == nil && terminal.Type == "response.completed" {
			_, _ = w.Write(terminal.Response)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

type fakeAuth struct {
	token     string
	refreshes int
}

func (a *fakeAuth) Token(context.Context) (string, string, error) { return a.token, "account", nil }
func (a *fakeAuth) RefreshIfUnchanged(_ context.Context, token string) error {
	if token == a.token {
		a.token = "new-token"
		a.refreshes++
	}
	return nil
}

func TestEffortsAndInputBoundary(t *testing.T) {
	for effort, want := range map[string]mode{"none": {"instant", ""}, "low": {"thinking", "min"}, "medium": {"thinking", "standard"}, "high": {"thinking", "extended"}, "xhigh": {"thinking", "max"}, "max": {"pro", "standard"}} {
		body := `{"model":"` + ModelSlug + `","input":"hello","reasoning":{"effort":"` + effort + `"}}`
		p, err := prepare([]byte(body))
		if err != nil || p.mode != want {
			t.Fatalf("%s: mode=%+v err=%v", effort, p.mode, err)
		}
	}
	for _, input := range []string{`[{"type":"message","content":[{"type":"input_image","image_url":"data:..."}]}]`, `[{"type":"message","content":[{"type":"input_audio","audio":"..."}]}]`} {
		body := `{"model":"` + ModelSlug + `","input":` + input + `}`
		if _, err := prepare([]byte(body)); err == nil {
			t.Fatalf("accepted unsupported input: %s", input)
		}
	}
}

func TestCodexNamespaceAndBuiltInToolDeclarations(t *testing.T) {
	body := `{"model":"` + ModelSlug + `","input":"hello","tools":[{"type":"custom","name":"exec","format":{"type":"text"}},{"type":"namespace","name":"clock","tools":[{"type":"function","name":"sleep","parameters":{"type":"object"}}]},{"type":"web_search"}]}`
	p, err := prepare([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.tools) != 2 || p.tools["exec"].Type != "custom" || p.tools["clock.sleep"].Type != "function" {
		t.Fatalf("tools=%v", p.tools)
	}
}

func TestStreamingTextAndToolResults(t *testing.T) {
	auth := &fakeAuth{token: "secret-token"}
	textAdapter := Adapter{Auth: auth, Run: func(_ context.Context, in transportInput, onText func(string) error) error {
		if in.Lane != "instant" || in.AccessToken != "secret-token" {
			t.Fatalf("transport input: %+v", in)
		}
		if err := onText("Hello"); err != nil {
			return err
		}
		return onText(" world")
	}}
	body := `{"model":"` + ModelSlug + `","input":"Say hello","stream":true}`
	w := serveAdapterForTest(t, textAdapter, []byte(body))
	response := w.Result()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream response: %d %v", response.StatusCode, response.Header)
	}
	data := w.Body.String()
	for _, event := range []string{"response.created", "response.output_text.delta", "response.output_item.done", "response.completed", "Hello world"} {
		if !strings.Contains(data, event) {
			t.Fatalf("missing %s in %s", event, data)
		}
	}
	if strings.Contains(data, "secret-token") {
		t.Fatal("credential leaked to stream")
	}

	toolAdapter := Adapter{Auth: auth, Run: func(_ context.Context, in transportInput, onText func(string) error) error {
		if in.Lane != "thinking" || in.Effort != "extended" || !strings.Contains(in.Prompt, "tool_result") {
			t.Fatalf("tool transport input: %+v", in)
		}
		return onText(`{"text":"","tool_calls":[{"name":"functions.exec","arguments":{"input":"pwd"}}]}`)
	}}
	body = `{"model":"` + ModelSlug + `","input":[{"type":"custom_tool_call_output","call_id":"call_previous","output":"tool_result"}],"tools":[{"type":"custom","name":"functions.exec","description":"Execute","format":{"type":"text"}}],"tool_choice":"required","reasoning":{"effort":"high"}}`
	w = serveAdapterForTest(t, toolAdapter, []byte(body))
	var result struct {
		Output []struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			Input  string `json:"input"`
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("tool response: %d %s", w.Code, w.Body.String())
	}
	if len(result.Output) != 1 || result.Output[0].Type != "custom_tool_call" || result.Output[0].Name != "functions.exec" || result.Output[0].Input != "pwd" || result.Output[0].CallID == "" {
		t.Fatalf("tool output: %+v", result.Output)
	}
}

func TestRefreshOnlyBeforeInferenceDispatch(t *testing.T) {
	p, err := prepare([]byte(`{"model":"` + ModelSlug + `","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	auth := &fakeAuth{token: "old-token"}
	calls := 0
	runner := func(_ context.Context, in transportInput, onText func(string) error) error {
		calls++
		if calls == 1 {
			return &transportError{Code: "catalog", Status: 401, Dispatched: false}
		}
		if in.AccessToken != "new-token" {
			t.Fatalf("stale token on second call")
		}
		return onText("ok")
	}
	if err := runWithRefresh(context.Background(), auth, runner, p, func(string) error { return nil }); err != nil || calls != 2 || auth.refreshes != 1 {
		t.Fatalf("refresh: err=%v calls=%d refreshes=%d", err, calls, auth.refreshes)
	}
	calls, auth.refreshes = 0, 0
	runner = func(context.Context, transportInput, func(string) error) error {
		calls++
		return &transportError{Code: "inference", Status: 401, Dispatched: true}
	}
	if err := runWithRefresh(context.Background(), auth, runner, p, func(string) error { return nil }); err == nil || calls != 1 || auth.refreshes != 0 {
		t.Fatalf("replayed dispatched inference: err=%v calls=%d refreshes=%d", err, calls, auth.refreshes)
	}
}
