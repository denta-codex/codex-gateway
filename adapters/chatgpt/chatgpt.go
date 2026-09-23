// Package chatgpt adds consumer ChatGPT conversation modes to the gateway.
// The conversation protocol is undocumented and deliberately isolated here.
package chatgpt

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/denta-codex/codex-gateway/adapter"
)

const ModelSlug = "subscription-chatgpt/chatgpt"
const maxPayload = 8 << 20

type Adapter struct {
	Auth adapter.TokenSource
	Run  Runner // nil selects the bundled transport
}

func (a *Adapter) SetSubscriptionAuth(source adapter.TokenSource) { a.Auth = source }

func (Adapter) Namespace() string { return "subscription-chatgpt" }

func (Adapter) Models() []adapter.Model {
	return []adapter.Model{{Slug: ModelSlug, TemplateNative: true, Catalog: json.RawMessage(`{
  "slug":"subscription-chatgpt/chatgpt",
  "display_name":"Subscription ChatGPT",
  "description":"ChatGPT Instant, Thinking, and Pro through Grace's existing login.",
  "visibility":"list", "supported_in_api":true,
  "context_window":32000, "max_context_window":32000, "input_modalities":["text"],
  "supported_reasoning_levels":[
    {"effort":"none","description":"Instant"},
    {"effort":"low","description":"Thinking Light"},
    {"effort":"medium","description":"Thinking Standard"},
    {"effort":"high","description":"Thinking Extended"},
    {"effort":"xhigh","description":"Thinking Heavy"},
    {"effort":"max","description":"Pro Standard"}
  ],
  "default_reasoning_level":"none", "supports_parallel_tool_calls":true,
  "supports_reasoning_summaries":false, "supports_search_tool":false,
  "supports_reasoning_summary_parameter":false, "supports_reasoning_effort_updates":false,
  "supports_image_detail_original":false, "support_verbosity":false,
  "use_responses_lite":false, "web_search_tool_type":null,
  "prefer_websockets":false, "tool_mode":"code_mode_only",
  "shell_type":"shell_command", "apply_patch_tool_type":"freeform",
  "model_messages":{}, "base_instructions":"You are Codex, a coding agent. Follow the user's request and use tools when needed.",
  "experimental_supported_tools":[], "service_tiers":[], "additional_speed_tiers":[],
  "availability_nux":null, "available_access_programs":{}, "priority":100
}`)}}
}

type mode struct{ lane, effort string }

func selectMode(effort string) (mode, error) {
	switch effort {
	case "", "none":
		return mode{lane: "instant"}, nil
	case "low":
		return mode{"thinking", "min"}, nil
	case "medium":
		return mode{"thinking", "standard"}, nil
	case "high":
		return mode{"thinking", "extended"}, nil
	case "xhigh":
		return mode{"thinking", "max"}, nil
	case "max":
		return mode{"pro", "standard"}, nil
	default:
		return mode{}, errors.New("unsupported ChatGPT reasoning effort")
	}
}

type request struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions"`
	Input             json.RawMessage `json:"input"`
	Tools             []tool          `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	Stream            bool            `json:"stream"`
	Reasoning         struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Format      json.RawMessage `json:"format"`
	Tools       []tool          `json:"tools,omitempty"`
}

type prepared struct {
	mode     mode
	prompt   string
	tools    map[string]tool
	required bool
	parallel bool
	stream   bool
}

func prepare(body []byte) (prepared, error) {
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		return prepared{}, errors.New("Responses body must be JSON")
	}
	if req.Model != ModelSlug {
		return prepared{}, errors.New("unknown Subscription ChatGPT model")
	}
	if len(req.Input) == 0 || !json.Valid(req.Input) {
		return prepared{}, errors.New("ChatGPT input is required")
	}
	if hasUnsupportedInput(req.Input) {
		return prepared{}, errors.New("Subscription ChatGPT currently accepts text and tool results only")
	}
	selected, err := selectMode(req.Reasoning.Effort)
	if err != nil {
		return prepared{}, err
	}
	choice, chosenName, required, err := parseToolChoice(req.ToolChoice)
	if err != nil {
		return prepared{}, err
	}
	tools := make(map[string]tool)
	addTool := func(t tool) error {
		if t.Type != "function" && t.Type != "custom" {
			// Codex can send built-in declarations even when this catalog row
			// does not advertise those capabilities. They are not local tools.
			return nil
		}
		if t.Name == "" || tools[t.Name].Name != "" {
			return errors.New("invalid or duplicate tool name")
		}
		if choice == "none" || chosenName != "" && chosenName != t.Name {
			return nil
		}
		tools[t.Name] = t
		return nil
	}
	for _, t := range req.Tools {
		if t.Type == "namespace" {
			if t.Name == "" {
				return prepared{}, errors.New("invalid tool namespace")
			}
			for _, child := range t.Tools {
				child.Name = t.Name + "." + child.Name
				if err := addTool(child); err != nil {
					return prepared{}, err
				}
			}
			continue
		}
		if err := addTool(t); err != nil {
			return prepared{}, err
		}
	}
	if chosenName != "" && tools[chosenName].Name == "" {
		return prepared{}, errors.New("selected tool is undeclared")
	}
	if required && len(tools) == 0 {
		return prepared{}, errors.New("no callable tools are available")
	}
	parallel := req.ParallelToolCalls == nil || *req.ParallelToolCalls
	instruction := "Continue the supplied conversation as the assistant. Return the answer as plain text. No tools are available this turn."
	if len(tools) > 0 {
		instruction = `Continue the supplied conversation as the assistant. Return ONLY a JSON object {"text":"message to user, or empty","tool_calls":[{"name":"exact declared tool name","arguments":{}}]}. No markdown fences. To use a tool, emit a tool_call and wait for its result in the next turn. Never claim to execute tools yourself. For freeform tools arguments must be {"input":"exact tool input"}. Empty tool_calls means final answer.`
		if required {
			instruction += " You MUST call an allowed tool this turn."
		}
		if !parallel {
			instruction += " Call at most one tool this turn."
		}
	}
	context, _ := json.Marshal(map[string]any{"instructions": req.Instructions, "input": req.Input, "tools": tools})
	prompt := instruction + "\nConversation and tool declarations:\n" + string(context)
	if len(prompt) > maxPayload {
		return prepared{}, errors.New("ChatGPT input exceeds 8 MiB")
	}
	return prepared{selected, prompt, tools, required, parallel, req.Stream}, nil
}

func hasUnsupportedInput(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return true
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if kind, _ := x["type"].(string); strings.Contains(kind, "image") || strings.Contains(kind, "audio") || strings.Contains(kind, "file") || strings.Contains(kind, "video") {
				return true
			}
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range x {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(value)
}

func parseToolChoice(raw json.RawMessage) (choice, name string, required bool, err error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "auto", "", false, nil
	}
	if json.Unmarshal(raw, &choice) == nil {
		switch choice {
		case "auto":
			return choice, "", false, nil
		case "none":
			return choice, "", false, nil
		case "required":
			return choice, "", true, nil
		}
		return "", "", false, errors.New("unsupported tool choice")
	}
	var selected struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &selected) != nil || selected.Name == "" || selected.Type != "function" && selected.Type != "custom" {
		return "", "", false, errors.New("unsupported tool choice")
	}
	return selected.Type, selected.Name, true, nil
}

func (a Adapter) ServeResponses(w http.ResponseWriter, r *http.Request, body []byte) {
	a.serveResponses(w, r, body)
}
