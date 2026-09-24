package modal

import (
	"encoding/json"

	"github.com/denta-codex/codex-gateway/adapter"
)

const (
	Namespace = "modal"

	DeepSeekModel = "andrew-61005--ep-codex-tasks-shared-deepseek-server.us-west.modal.direct"
	GLMFlashModel = "andrew-61005--ep-codex-tasks-shared-glm-server.us-west.modal.direct"
	KimiModel     = "andrew-61005--ep-codex-tasks-shared-kimi-server.us-west.modal.direct"
)

type modelSpec struct {
	id           string
	name         string
	modalities   string
	context      int
	efforts      []string
	defaultLevel string
}

var modelSpecs = []modelSpec{
	{DeepSeekModel, "DeepSeek V4.1 Flash", `["text","image"]`, 1_048_576, []string{"none", "low", "high", "xhigh", "max"}, "high"},
	{GLMFlashModel, "GLM 5.3 Flash", `["text","image"]`, 1_048_576, []string{"low", "high", "max"}, "high"},
	{KimiModel, "Kimi K3", `["text","image"]`, 1_048_576, []string{"low", "high", "max"}, "high"},
}

func models() []adapter.Model {
	rows := make([]adapter.Model, 0, len(modelSpecs))
	for _, spec := range modelSpecs {
		levels := make([]map[string]string, 0, len(spec.efforts))
		for _, effort := range spec.efforts {
			levels = append(levels, map[string]string{"effort": effort, "description": effort})
		}
		row, err := json.Marshal(map[string]any{
			"slug":                                 Namespace + "/" + spec.id,
			"display_name":                         "Modal " + spec.name,
			"description":                          spec.name + " through Modal Chat Completions.",
			"base_instructions":                    "You are Codex, a coding agent. Follow the user's request and use tools when needed.",
			"model_messages":                       map[string]any{},
			"visibility":                           "list",
			"supported_in_api":                     true,
			"context_window":                       spec.context,
			"max_context_window":                   spec.context,
			"input_modalities":                     json.RawMessage(spec.modalities),
			"supported_reasoning_levels":           levels,
			"default_reasoning_level":              spec.defaultLevel,
			"default_reasoning_summary":            "none",
			"supports_parallel_tool_calls":         true,
			"supports_reasoning_summaries":         false,
			"supports_search_tool":                 false,
			"supports_reasoning_summary_parameter": false,
			"supports_reasoning_effort_updates":    false,
			"supports_image_detail_original":       false,
			"support_verbosity":                    false,
			"use_responses_lite":                   false,
			"web_search_tool_type":                 nil,
			"prefer_websockets":                    false,
			"tool_mode":                            "code_mode_only",
			"shell_type":                           "shell_command",
			"apply_patch_tool_type":                "freeform",
			"experimental_supported_tools":         []any{},
			"service_tiers":                        []any{},
			"additional_speed_tiers":               []any{},
			"availability_nux":                     nil,
			"available_access_programs":            map[string]any{},
			"priority":                             90,
		})
		if err != nil {
			panic(err)
		}
		rows = append(rows, adapter.Model{Slug: Namespace + "/" + spec.id, Catalog: row, TemplateNative: true})
	}
	return rows
}

func knownModel(id string) bool {
	for _, spec := range modelSpecs {
		if spec.id == id {
			return true
		}
	}
	return false
}
