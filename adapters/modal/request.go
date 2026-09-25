package modal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	errEncryptedContextCompaction = errors.New("encrypted context compaction is unsupported by the Modal adapter")
	errResponsesCompaction        = errors.New("Responses compaction is unsupported by the Modal adapter")
)

const maxTranslatedPayload = 16 << 20

var invalidToolName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

type toolKind string

const (
	functionTool toolKind = "function"
	customTool   toolKind = "custom"
)

type toolIdentity struct {
	Name      string
	Namespace string
	WireName  string
	Kind      toolKind
}

type toolRegistry struct {
	byLogical map[string]toolIdentity
	byWire    map[string]toolIdentity
	permitted map[string]bool
}

func newToolRegistry() *toolRegistry {
	return &toolRegistry{byLogical: map[string]toolIdentity{}, byWire: map[string]toolIdentity{}}
}

func logicalToolKey(namespace, name string) string { return namespace + "\x00" + name }

func wireToolName(namespace, name string) string {
	logical := name
	if namespace != "" && namespace != "functions" {
		logical = namespace + "__" + name
	}
	clean := invalidToolName.ReplaceAllString(logical, "_")
	if clean == "" {
		clean = "tool"
	}
	if len(clean) <= 64 {
		return clean
	}
	digest := sha256.Sum256([]byte(logical))
	return clean[:49] + "_" + hex.EncodeToString(digest[:7])
}

func (r *toolRegistry) add(identity toolIdentity) error {
	key := logicalToolKey(identity.Namespace, identity.Name)
	if previous, ok := r.byLogical[key]; ok {
		if previous.Kind != identity.Kind {
			return fmt.Errorf("tool %q was declared with conflicting kinds", identity.Name)
		}
		return nil
	}
	if previous, ok := r.byWire[identity.WireName]; ok && logicalToolKey(previous.Namespace, previous.Name) != key {
		return fmt.Errorf("tool names %q and %q collide after Chat translation", previous.Name, identity.Name)
	}
	r.byLogical[key] = identity
	r.byWire[identity.WireName] = identity
	return nil
}

func (r *toolRegistry) resolve(namespace, name string, kind toolKind) toolIdentity {
	if identity, ok := r.byLogical[logicalToolKey(namespace, name)]; ok {
		return identity
	}
	if namespace == "functions" {
		if identity, ok := r.byLogical[logicalToolKey("", name)]; ok {
			return identity
		}
	}
	return toolIdentity{Name: name, Namespace: namespace, WireName: wireToolName(namespace, name), Kind: kind}
}

func (r *toolRegistry) permitOnly(wireNames ...string) {
	r.permitted = make(map[string]bool, len(wireNames))
	for _, name := range wireNames {
		r.permitted[name] = true
	}
}

func (r *toolRegistry) permits(wireName string) bool {
	return r.permitted == nil || r.permitted[wireName]
}

func (r *toolRegistry) filterTools(tools []any) []any {
	if r.permitted == nil {
		return tools
	}
	filtered := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if r.permitted[name] {
			filtered = append(filtered, raw)
		}
	}
	return filtered
}

type responsesRequest struct {
	Model             string            `json:"model"`
	Instructions      *string           `json:"instructions"`
	Input             json.RawMessage   `json:"input"`
	Tools             []json.RawMessage `json:"tools"`
	ToolChoice        json.RawMessage   `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls"`
	Stream            bool              `json:"stream"`
	PreviousResponse  string            `json:"previous_response_id"`
	MaxOutputTokens   *int              `json:"max_output_tokens"`
	Temperature       *float64          `json:"temperature"`
	TopP              *float64          `json:"top_p"`
	Stop              json.RawMessage   `json:"stop"`
	PresencePenalty   *float64          `json:"presence_penalty"`
	FrequencyPenalty  *float64          `json:"frequency_penalty"`
	Reasoning         *struct {
		Effort  string `json:"effort"`
		Summary string `json:"summary"`
	} `json:"reasoning"`
	Text json.RawMessage `json:"text"`
}

type translatedRequest struct {
	Model    string
	Stream   bool
	Body     []byte
	Registry *toolRegistry
}

func translateRequest(body []byte) (translatedRequest, error) {
	if len(body) > maxTranslatedPayload {
		return translatedRequest{}, errors.New("Responses request exceeds Modal adapter limit")
	}
	var input responsesRequest
	if err := json.Unmarshal(body, &input); err != nil {
		return translatedRequest{}, errors.New("Responses body must be JSON")
	}
	if !strings.HasPrefix(input.Model, Namespace+"/") {
		return translatedRequest{}, errors.New("Modal model must use the modal namespace")
	}
	model := strings.TrimPrefix(input.Model, Namespace+"/")
	if !knownModel(model) {
		return translatedRequest{}, errors.New("unknown Modal model")
	}
	if input.PreviousResponse != "" {
		return translatedRequest{}, errors.New("Modal adapter requires self-contained input; previous_response_id is unsupported")
	}
	registry := newToolRegistry()
	additionalTools, err := extractAdditionalTools(input.Input)
	if err != nil {
		return translatedRequest{}, err
	}
	allTools := append(append([]json.RawMessage{}, input.Tools...), additionalTools...)
	chatTools, err := translateTools(allTools, registry)
	if err != nil {
		return translatedRequest{}, err
	}
	messages, err := translateMessages(input.Instructions, input.Input, registry)
	if err != nil {
		return translatedRequest{}, err
	}
	if len(messages) == 0 {
		return translatedRequest{}, errors.New("Responses input is required")
	}
	chatBody := map[string]any{
		"model":          model,
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	choice, err := translateToolChoice(input.ToolChoice, registry)
	if err != nil {
		return translatedRequest{}, err
	}
	chatTools = registry.filterTools(chatTools)
	if len(chatTools) > 0 {
		chatBody["tools"] = chatTools
		if choice != nil {
			chatBody["tool_choice"] = choice
		}
		if input.ParallelToolCalls != nil {
			chatBody["parallel_tool_calls"] = *input.ParallelToolCalls
		}
	} else if choice == "required" {
		return translatedRequest{}, errors.New("tool_choice requires at least one supported tool")
	}
	if input.MaxOutputTokens != nil {
		if *input.MaxOutputTokens <= 0 {
			return translatedRequest{}, errors.New("max_output_tokens must be positive")
		}
		chatBody["max_tokens"] = *input.MaxOutputTokens
	}
	if input.Temperature != nil {
		chatBody["temperature"] = *input.Temperature
	}
	if input.TopP != nil {
		chatBody["top_p"] = *input.TopP
	}
	if input.PresencePenalty != nil {
		chatBody["presence_penalty"] = *input.PresencePenalty
	}
	if input.FrequencyPenalty != nil {
		chatBody["frequency_penalty"] = *input.FrequencyPenalty
	}
	if len(input.Stop) > 0 && string(input.Stop) != "null" {
		var stop any
		if err := json.Unmarshal(input.Stop, &stop); err != nil {
			return translatedRequest{}, errors.New("invalid stop value")
		}
		chatBody["stop"] = stop
	}
	if input.Reasoning != nil && input.Reasoning.Effort != "" {
		effort := input.Reasoning.Effort
		if effort == "ultra" {
			effort = "max"
		}
		chatBody["reasoning_effort"] = effort
	}
	if format := translateTextFormat(input.Text); format != nil {
		chatBody["response_format"] = format
	}
	encoded, err := json.Marshal(chatBody)
	if err != nil {
		return translatedRequest{}, err
	}
	if len(encoded) > maxTranslatedPayload {
		return translatedRequest{}, errors.New("translated Chat request exceeds Modal adapter limit")
	}
	return translatedRequest{Model: model, Stream: input.Stream, Body: encoded, Registry: registry}, nil
}

func translateTools(rawTools []json.RawMessage, registry *toolRegistry) ([]any, error) {
	out := make([]any, 0, len(rawTools))
	var add func(map[string]any, string) error
	add = func(tool map[string]any, namespace string) error {
		kind, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if kind == "namespace" {
			ns, _ := tool["name"].(string)
			children, _ := tool["tools"].([]any)
			if ns == "" || children == nil {
				return errors.New("invalid tool namespace")
			}
			if ns == "functions" {
				ns = ""
			}
			for _, child := range children {
				item, ok := child.(map[string]any)
				if !ok {
					return errors.New("invalid namespaced tool")
				}
				if err := add(item, ns); err != nil {
					return err
				}
			}
			return nil
		}
		if kind == "web_search" || kind == "web_search_preview" || kind == "image_generation" || kind == "file_search" || kind == "code_interpreter" || kind == "mcp" || kind == "computer" || kind == "computer_use_preview" || kind == "tool_search" {
			return nil
		}
		if name == "" {
			return nil
		}
		if kind != "function" && kind != "custom" {
			return fmt.Errorf("unsupported tool type %q", kind)
		}
		identity := toolIdentity{Name: name, Namespace: namespace, WireName: wireToolName(namespace, name), Kind: functionTool}
		parameters := tool["parameters"]
		if kind == "custom" {
			identity.Kind = customTool
			parameters = map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"input": map[string]any{"type": "string", "description": "Raw freeform input for this tool."}},
				"required":             []string{"input"},
				"additionalProperties": false,
			}
		}
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if previous, ok := registry.byLogical[logicalToolKey(identity.Namespace, identity.Name)]; ok {
			if previous.Kind != identity.Kind {
				return fmt.Errorf("tool %q was declared with conflicting kinds", identity.Name)
			}
			return nil
		}
		if err := registry.add(identity); err != nil {
			return err
		}
		fn := map[string]any{"name": identity.WireName, "parameters": parameters}
		if description, ok := tool["description"].(string); ok && description != "" {
			fn["description"] = description
		}
		if strict, ok := tool["strict"].(bool); ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
		return nil
	}
	for _, raw := range rawTools {
		var tool map[string]any
		if err := json.Unmarshal(raw, &tool); err != nil {
			return nil, errors.New("invalid tool declaration")
		}
		if err := add(tool, ""); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func extractAdditionalTools(rawInput json.RawMessage) ([]json.RawMessage, error) {
	if len(rawInput) == 0 || string(rawInput) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(rawInput, &text) == nil {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil {
		return nil, errors.New("Responses input must be a string or item array")
	}
	var tools []json.RawMessage
	for _, raw := range items {
		var item struct {
			Type  string            `json:"type"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, errors.New("invalid Responses input item")
		}
		if item.Type == "additional_tools" {
			if item.Tools == nil {
				return nil, errors.New("additional_tools item needs a tool array")
			}
			tools = append(tools, item.Tools...)
		}
	}
	return tools, nil
}

func translateToolChoice(raw json.RawMessage, registry *toolRegistry) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		switch text {
		case "auto", "required":
			return text, nil
		case "none":
			registry.permitOnly()
			return text, nil
		default:
			return nil, errors.New("unsupported tool_choice")
		}
	}
	var selected struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Mode      string `json:"mode"`
		Tools     []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"tools"`
	}
	if json.Unmarshal(raw, &selected) != nil {
		return nil, errors.New("unsupported tool_choice")
	}
	if selected.Type == "allowed_tools" {
		if (selected.Mode != "auto" && selected.Mode != "required") || len(selected.Tools) == 0 {
			return nil, errors.New("invalid allowed_tools choice")
		}
		wireNames := make([]string, 0, len(selected.Tools))
		for _, choice := range selected.Tools {
			if choice.Type != "function" && choice.Type != "custom" {
				continue
			}
			identity, err := selectTool(registry, choice.Type, choice.Namespace, choice.Name)
			if err != nil {
				return nil, err
			}
			wireNames = append(wireNames, identity.WireName)
		}
		if len(wireNames) == 0 {
			registry.permitOnly()
			return "none", nil
		}
		registry.permitOnly(wireNames...)
		if selected.Mode == "required" && len(wireNames) == 1 {
			return map[string]any{"type": "function", "function": map[string]any{"name": wireNames[0]}}, nil
		}
		return selected.Mode, nil
	}
	if selected.Name == "" || (selected.Type != "function" && selected.Type != "custom") {
		return nil, errors.New("unsupported tool_choice")
	}
	identity, err := selectTool(registry, selected.Type, selected.Namespace, selected.Name)
	if err != nil {
		return nil, err
	}
	registry.permitOnly(identity.WireName)
	return map[string]any{"type": "function", "function": map[string]any{"name": identity.WireName}}, nil
}

func selectTool(registry *toolRegistry, selectedType, namespace, name string) (toolIdentity, error) {
	if name == "" {
		return toolIdentity{}, errors.New("selected tool is missing or ambiguous")
	}
	if namespace == "functions" {
		namespace = ""
	}
	if namespace != "" {
		identity, ok := registry.byLogical[logicalToolKey(namespace, name)]
		if !ok || string(identity.Kind) != selectedType {
			return toolIdentity{}, errors.New("selected tool is missing or ambiguous")
		}
		return identity, nil
	}
	matches := make([]toolIdentity, 0, 1)
	for _, identity := range registry.byLogical {
		if identity.Name == name && string(identity.Kind) == selectedType {
			matches = append(matches, identity)
		}
	}
	if len(matches) != 1 {
		return toolIdentity{}, errors.New("selected tool is missing or ambiguous")
	}
	return matches[0], nil
}

func translateTextFormat(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text struct {
		Format map[string]any `json:"format"`
	}
	if json.Unmarshal(raw, &text) != nil || text.Format == nil {
		return nil
	}
	switch text.Format["type"] {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		inner := map[string]any{}
		for _, key := range []string{"name", "description", "schema", "strict"} {
			if value, ok := text.Format[key]; ok {
				inner[key] = value
			}
		}
		if inner["name"] == nil {
			inner["name"] = "response"
		}
		return map[string]any{"type": "json_schema", "json_schema": inner}
	}
	return nil
}

func translateMessages(instructions *string, rawInput json.RawMessage, registry *toolRegistry) ([]any, error) {
	messages := make([]any, 0)
	if instructions != nil && *instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": *instructions})
	}
	if len(rawInput) == 0 || string(rawInput) == "null" {
		return messages, nil
	}
	var text string
	if json.Unmarshal(rawInput, &text) == nil {
		return append(messages, map[string]any{"role": "user", "content": text}), nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(rawInput, &items); err != nil {
		return nil, errors.New("Responses input must be a string or item array")
	}
	var pendingReasoning strings.Builder
	var activeAssistant map[string]any
	startAssistant := func() map[string]any {
		if activeAssistant != nil {
			return activeAssistant
		}
		activeAssistant = map[string]any{"role": "assistant", "content": ""}
		if pendingReasoning.Len() > 0 {
			activeAssistant["reasoning_content"] = pendingReasoning.String()
			pendingReasoning.Reset()
		}
		messages = append(messages, activeAssistant)
		return activeAssistant
	}
	barrier := func() { activeAssistant = nil }
	for _, raw := range items {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, errors.New("invalid Responses input item")
		}
		kind, _ := item["type"].(string)
		role, _ := item["role"].(string)
		if kind == "" && role != "" {
			kind = "message"
		}
		switch kind {
		case "reasoning":
			for _, field := range []string{"summary", "content"} {
				parts, _ := item[field].([]any)
				for _, part := range parts {
					if block, ok := part.(map[string]any); ok {
						if value, ok := block["text"].(string); ok {
							pendingReasoning.WriteString(value)
						}
					}
				}
			}
		case "message", "agent_message":
			content, err := messageContent(item["content"], role == "assistant")
			if err != nil {
				return nil, err
			}
			switch role {
			case "assistant":
				if activeAssistant != nil {
					if previous, _ := activeAssistant["content"].(string); previous != "" || activeAssistant["tool_calls"] != nil {
						barrier()
					}
				}
				assistant := startAssistant()
				assistant["content"] = content
			case "developer", "system":
				barrier()
				messages = append(messages, map[string]any{"role": "system", "content": content})
			case "user", "":
				barrier()
				messages = append(messages, map[string]any{"role": "user", "content": content})
			default:
				return nil, fmt.Errorf("unsupported message role %q", role)
			}
		case "function_call", "custom_tool_call":
			name, _ := item["name"].(string)
			callID, _ := item["call_id"].(string)
			namespace, _ := item["namespace"].(string)
			if name == "" || callID == "" {
				return nil, errors.New("historical tool call needs name and call_id")
			}
			toolType := functionTool
			arguments, _ := item["arguments"].(string)
			if kind == "custom_tool_call" {
				toolType = customTool
				input, _ := item["input"].(string)
				encoded, _ := json.Marshal(map[string]string{"input": input})
				arguments = string(encoded)
			}
			if arguments == "" {
				arguments = "{}"
			}
			identity := registry.resolve(namespace, name, toolType)
			assistant := startAssistant()
			calls, _ := assistant["tool_calls"].([]any)
			calls = append(calls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": identity.WireName, "arguments": arguments}})
			assistant["tool_calls"] = calls
		case "function_call_output", "custom_tool_call_output":
			barrier()
			callID, _ := item["call_id"].(string)
			if callID == "" {
				return nil, errors.New("tool output needs call_id")
			}
			output, err := toolOutputText(item["output"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
		case "additional_tools":
			// Desktop Responses-lite requests carry deferred tool declarations here.
			// translateRequest merges them into the Chat tool catalog.
			continue
		case "context_compaction":
			// Codex local compaction uses this as a boundary marker; the readable
			// summary is carried by a separate message item.
			if _, encrypted := item["encrypted_content"].(string); !encrypted {
				continue
			}
			return nil, errEncryptedContextCompaction
		case "compaction", "compaction_trigger":
			return nil, fmt.Errorf("%w: item %q", errResponsesCompaction, kind)
		default:
			return nil, fmt.Errorf("Responses item %q is unsupported by the Modal adapter", kind)
		}
	}
	if pendingReasoning.Len() > 0 {
		messages = append(messages, map[string]any{"role": "assistant", "content": "", "reasoning_content": pendingReasoning.String()})
	}
	return messages, nil
}

func messageContent(value any, assistant bool) (any, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	blocks, ok := value.([]any)
	if !ok {
		return nil, errors.New("message content must be text or content parts")
	}
	parts := make([]any, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("invalid message content part")
		}
		kind, _ := block["type"].(string)
		switch kind {
		case "input_text", "output_text", "text":
			text, _ := block["text"].(string)
			parts = append(parts, map[string]any{"type": "text", "text": text})
		case "refusal":
			text, _ := block["refusal"].(string)
			parts = append(parts, map[string]any{"type": "text", "text": text})
		case "input_image":
			if assistant {
				return nil, errors.New("assistant image history is unsupported")
			}
			url, _ := block["image_url"].(string)
			if url == "" {
				return nil, errors.New("file_id image inputs are unsupported")
			}
			image := map[string]any{"url": url}
			if detail, ok := block["detail"].(string); ok && detail != "" && detail != "original" {
				image["detail"] = detail
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
		case "input_audio", "input_video", "input_file":
			return nil, fmt.Errorf("content part %q is unsupported", kind)
		default:
			return nil, fmt.Errorf("content part %q is unsupported", kind)
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 {
		if part, ok := parts[0].(map[string]any); ok && part["type"] == "text" {
			return part["text"], nil
		}
	}
	return parts, nil
}

func toolOutputText(value any) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := value.([]any)
	if !ok {
		encoded, err := json.Marshal(value)
		return string(encoded), err
	}
	var output strings.Builder
	for _, raw := range parts {
		block, ok := raw.(map[string]any)
		if !ok {
			return "", errors.New("invalid tool output part")
		}
		switch block["type"] {
		case "input_text", "output_text", "text":
			text, _ := block["text"].(string)
			output.WriteString(text)
		case "refusal":
			text, _ := block["refusal"].(string)
			output.WriteString(text)
		case "input_image":
			output.WriteString("[image output omitted by Chat translation]")
		case "encrypted_content":
			return "", errors.New("encrypted tool output is unsupported")
		default:
			return "", errors.New("unsupported tool output content")
		}
	}
	return output.String(), nil
}
