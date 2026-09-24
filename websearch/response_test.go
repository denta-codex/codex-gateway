package websearch

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseSearchResponseUsesCompletedTextAndSafeAnnotations(t *testing.T) {
	body := "event: response.completed\n" + `data: {"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation","title":"Primary","url":"https://example.com/source"},{"type":"url_citation","title":"Bad","url":"javascript:alert(1)"}]}]}]}}` + "\n\n"
	response := httpResponse(http.StatusOK, "", body)
	outcome, err := parseSearchResponse(response)
	if err != nil || outcome.text != "answer" || len(outcome.sources) != 1 || outcome.sources[0].URL != "https://example.com/source" {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
}

func TestParseSearchResponseRejectsMalformedAndOversizedStreams(t *testing.T) {
	malformed := httpResponse(http.StatusOK, "text/event-stream", "data: not-json\n\n")
	if _, err := parseSearchResponse(malformed); err == nil {
		t.Fatal("malformed SSE was accepted")
	}
	oversized := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxSidecarBytes+1)))}
	if _, err := parseSearchResponse(oversized); err == nil {
		t.Fatal("oversized SSE was accepted")
	}
	failed := httpResponse(http.StatusOK, "text/event-stream", "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"private\"}}}\n\n")
	if _, err := parseSearchResponse(failed); err == nil {
		t.Fatal("failed search stream was accepted")
	}
}

func TestBatchedQueriesAreBounded(t *testing.T) {
	queries, err := queriesFrom(map[string]any{"queries": []any{"one", "two", "three", "four", "five"}})
	if err != nil || len(queries) != maxQueries || queries[3] != "four" {
		t.Fatalf("queries=%v err=%v", queries, err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	auth := &fakeAuth{token: "secret-token", account: "account"}
	sends := 0
	service := serviceFor(t, auth, func(request *http.Request) (*http.Response, error) {
		sends++
		if request.URL.Host != "chatgpt.com" {
			t.Fatalf("credential request escaped ChatGPT: %s", request.URL)
		}
		response := httpResponse(http.StatusFound, "text/plain", "redirect")
		response.Header.Set("Location", "https://example.com/steal")
		response.Request = request
		return response, nil
	}, nil)
	outcome := service.search(context.Background(), "query", map[string]any{"type": "web_search"})
	if sends != 1 || !strings.Contains(outcome.err, "HTTP 302") {
		t.Fatalf("sends=%d outcome=%#v", sends, outcome)
	}
}

func TestHostedSearchRequiresReplayableInput(t *testing.T) {
	_, enabled, err := parseRequest([]byte(`{"model":"modal/model","tools":[{"type":"web_search"}]}`))
	if err == nil || enabled || !strings.Contains(err.Error(), "requires string or array") {
		t.Fatalf("enabled=%v err=%v", enabled, err)
	}
}
