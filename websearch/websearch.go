// Package websearch lets a routed Responses adapter borrow ChatGPT's hosted
// web_search tool without exposing the ChatGPT credential to that adapter's
// upstream. Adapters opt in explicitly by wrapping their Responses round trip.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/denta-codex/codex-gateway/adapter"
)

const (
	DefaultModel       = "gpt-5.6-luna"
	DefaultReasoning   = "low"
	DefaultMaxSearches = 3
	DefaultTimeout     = 60 * time.Second

	maxBackendBytes = 16 << 20
	maxQueries      = 4
)

const searchInstructions = "You are a web-search assistant. Use the web_search tool to find current information for the user's query, then reply with a concise, factual answer. End your reply with a `Sources:` section listing each source you used on its own line as `- Title: URL` (one per line)."

// Backend executes one stateless Responses iteration against a routed model.
// A provider adapter normally supplies a closure that captures its endpoint,
// credentials, and any request-scoped transport metadata.
type Backend interface {
	Responses(context.Context, []byte) (*http.Response, error)
}

// BackendFunc adapts a function to Backend.
type BackendFunc func(context.Context, []byte) (*http.Response, error)

func (f BackendFunc) Responses(ctx context.Context, body []byte) (*http.Response, error) {
	return f(ctx, body)
}

// Config controls the ChatGPT search helper. Zero values select the same core
// defaults as OpenCodex's ChatGPT-backed search sidecar.
type Config struct {
	Auth         adapter.TokenSource
	Client       *http.Client
	UpstreamBase string
	Model        string
	Reasoning    string
	MaxSearches  int
	Timeout      time.Duration
}

// Service is safe for concurrent use.
type Service struct {
	auth        adapter.TokenSource
	client      *http.Client
	endpoint    string
	model       string
	reasoning   string
	maxSearches int
	timeout     time.Duration
}

// New constructs a search service. Subscription credentials are deliberately
// restricted to chatgpt.com, and redirects are never followed.
func New(config Config) (*Service, error) {
	if config.Auth == nil {
		return nil, errors.New("web search requires subscription auth")
	}
	if config.UpstreamBase == "" {
		config.UpstreamBase = "https://chatgpt.com/backend-api/codex"
	}
	base, err := url.Parse(config.UpstreamBase)
	if err != nil || base.Scheme != "https" || !strings.EqualFold(base.Hostname(), "chatgpt.com") || base.Port() != "" || base.User != nil || base.Opaque != "" || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("web search upstream must be a fixed HTTPS chatgpt.com URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/responses"
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.Reasoning == "" {
		config.Reasoning = DefaultReasoning
	}
	if config.MaxSearches == 0 {
		config.MaxSearches = DefaultMaxSearches
	}
	if config.MaxSearches < 1 || config.MaxSearches > 10 {
		return nil, errors.New("web search max searches must be between 1 and 10")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.Timeout < time.Second {
		return nil, errors.New("web search timeout must be at least one second")
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Service{auth: config.Auth, client: &copyClient, endpoint: base.String(), model: config.Model, reasoning: config.Reasoning, maxSearches: config.MaxSearches, timeout: config.Timeout}, nil
}

type requestState struct {
	root       map[string]any
	hostedTool map[string]any
	baseTools  []any
	stream     bool
	model      string
}

// ServeResponses serves one Codex-facing request. Requests without a hosted
// web_search declaration are passed to backend byte-for-byte and relayed.
func (s *Service) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte, backend Backend) {
	if backend == nil {
		writeError(w, http.StatusInternalServerError, "web_search_backend_missing", "routed search backend is unavailable")
		return
	}
	state, enabled, err := parseRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_web_search_request", err.Error())
		return
	}
	if !enabled {
		response, backendErr := backend.Responses(r.Context(), body)
		if backendErr != nil {
			writeError(w, http.StatusBadGateway, "routed_model_unavailable", "routed model is unavailable")
			return
		}
		relayResponse(w, response)
		return
	}

	searches := 0
	failedQueries := map[string]bool{}
	for iteration := 0; iteration < s.maxSearches+2; iteration++ {
		forceAnswer := searches >= s.maxSearches
		state.setTools(!forceAnswer)
		iterationBody, marshalErr := json.Marshal(state.root)
		if marshalErr != nil {
			writeError(w, http.StatusInternalServerError, "web_search_request_failed", "could not encode routed request")
			return
		}
		response, backendErr := backend.Responses(r.Context(), iterationBody)
		if backendErr != nil {
			writeError(w, http.StatusBadGateway, "routed_model_unavailable", "routed model is unavailable")
			return
		}
		round, readErr := readRound(response, maxBackendBytes)
		if readErr != nil {
			writeError(w, http.StatusBadGateway, "routed_model_invalid_response", "routed model returned an invalid response")
			return
		}
		if round.status < 200 || round.status >= 300 {
			round.relay(w)
			return
		}
		searchCalls, hasRealTools := findCalls(round.output)
		if len(searchCalls) == 0 {
			round.relay(w)
			return
		}
		if forceAnswer {
			filtered := withoutSearchCalls(round.output)
			if len(filtered) == 0 {
				writeError(w, http.StatusBadGateway, "web_search_final_answer_missing", "routed model did not produce a final answer after web search")
				return
			}
			writeOutput(w, state.stream, state.model, filtered)
			return
		}
		if hasRealTools {
			writeOutput(w, state.stream, state.model, withoutSearchCalls(round.output))
			return
		}

		state.appendOutput(round.output)
		for _, call := range searchCalls {
			var result string
			if searches >= s.maxSearches {
				result = formatFailure("", "the per-turn web search limit was reached")
			} else {
				searches++
				queries, queryErr := queriesFrom(call.arguments)
				if queryErr != nil {
					result = formatFailure("", queryErr.Error())
				} else {
					results := make([]queryResult, 0, len(queries))
					for _, query := range queries {
						if failedQueries[query] {
							results = append(results, queryResult{query: query, outcome: searchOutcome{err: "this query already failed earlier in the turn"}})
							continue
						}
						outcome := s.search(r.Context(), query, state.hostedTool, r.Header)
						if outcome.err != "" {
							failedQueries[query] = true
						}
						results = append(results, queryResult{query: query, outcome: outcome})
					}
					result = formatResults(results)
				}
			}
			state.appendToolResult(call.callID, result)
		}
	}
	writeError(w, http.StatusBadGateway, "web_search_loop_exhausted", "routed model did not produce a final answer")
}

func parseRequest(body []byte) (*requestState, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, errors.New("Responses body must be JSON")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, false, errors.New("Responses body must contain one JSON value")
	}
	state := &requestState{root: root}
	state.stream, _ = root["stream"].(bool)
	state.model, _ = root["model"].(string)
	tools, _ := root["tools"].([]any)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			state.baseTools = append(state.baseTools, raw)
			continue
		}
		kind, _ := tool["type"].(string)
		if kind != "web_search" && kind != "web_search_preview" {
			if name, _ := tool["name"].(string); name == "web_search" {
				return nil, false, errors.New("web_search conflicts with an existing client tool")
			}
			state.baseTools = append(state.baseTools, raw)
			continue
		}
		if state.hostedTool != nil {
			return nil, false, errors.New("only one hosted web_search tool may be declared")
		}
		state.hostedTool = tool
	}
	if state.hostedTool != nil {
		switch root["input"].(type) {
		case string, []any:
		default:
			return nil, false, errors.New("web_search requires string or array Responses input")
		}
	}
	return state, state.hostedTool != nil, nil
}

func (s *requestState) setTools(includeSearch bool) {
	tools := append([]any(nil), s.baseTools...)
	if includeSearch {
		tools = append(tools, syntheticTool())
	}
	s.root["tools"] = tools
	choice, ok := s.root["tool_choice"].(map[string]any)
	if ok {
		name, _ := choice["name"].(string)
		kind, _ := choice["type"].(string)
		if name == "web_search" || kind == "web_search" || kind == "web_search_preview" {
			if includeSearch {
				s.root["tool_choice"] = "required"
			} else {
				s.root["tool_choice"] = "auto"
			}
		}
	}
	if !includeSearch {
		if choice, _ := s.root["tool_choice"].(string); choice == "required" && len(tools) == 0 {
			s.root["tool_choice"] = "none"
		}
	}
}

func syntheticTool() map[string]any {
	return map[string]any{
		"type": "function", "name": "web_search", "strict": false,
		"description": "Search the web for current, real-world, or post-training-cutoff information. Returns a concise answer with sources.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"query":   map[string]any{"type": "string", "description": "A focused natural-language search query."},
			"queries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Several related search queries."},
		}},
	}
}

func (s *requestState) input() []any {
	switch input := s.root["input"].(type) {
	case []any:
		return input
	case string:
		return []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": input}}}}
	default:
		return nil
	}
}

func (s *requestState) appendOutput(output []map[string]any) {
	input := s.input()
	for _, item := range output {
		input = append(input, item)
	}
	s.root["input"] = input
}

func (s *requestState) appendToolResult(callID, output string) {
	input := s.input()
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": output})
	s.root["input"] = input
}

type modelCall struct {
	callID    string
	arguments any
}

func findCalls(output []map[string]any) ([]modelCall, bool) {
	var searches []modelCall
	hasReal := false
	for _, item := range output {
		kind, _ := item["type"].(string)
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		name, _ := item["name"].(string)
		if name != "web_search" {
			hasReal = true
			continue
		}
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		if callID == "" {
			callID = fmt.Sprintf("web_search_%d", len(searches)+1)
			item["call_id"] = callID
		}
		arguments := item["arguments"]
		if kind == "custom_tool_call" {
			arguments = item["input"]
		}
		searches = append(searches, modelCall{callID: callID, arguments: arguments})
	}
	return searches, hasReal
}

func withoutSearchCalls(output []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(output))
	for _, item := range output {
		kind, _ := item["type"].(string)
		name, _ := item["name"].(string)
		if (kind == "function_call" || kind == "custom_tool_call") && name == "web_search" {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

func queriesFrom(raw any) ([]string, error) {
	var value map[string]any
	switch typed := raw.(type) {
	case string:
		if err := json.Unmarshal([]byte(typed), &value); err != nil {
			return nil, errors.New("the model supplied invalid web_search arguments")
		}
	case map[string]any:
		value = typed
	default:
		return nil, errors.New("the model supplied invalid web_search arguments")
	}
	var queries []string
	if query, _ := value["query"].(string); strings.TrimSpace(query) != "" {
		queries = append(queries, strings.TrimSpace(query))
	}
	if many, ok := value["queries"].([]any); ok {
		for _, rawQuery := range many {
			query, ok := rawQuery.(string)
			if ok && strings.TrimSpace(query) != "" {
				queries = append(queries, strings.TrimSpace(query))
			}
		}
	}
	if len(queries) == 0 {
		return nil, errors.New("the model called web_search without a usable query")
	}
	if len(queries) > maxQueries {
		queries = queries[:maxQueries]
	}
	return queries, nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": code, "message": message}})
}
