package relay

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// removeResponsesWebSearch applies an explicit administrator opt-out before
// protocol conversion. It preserves client-executed tools and conversation
// history; it does not relax the converter's tool-loss policy.
func removeResponsesWebSearch(request *dto.OpenAIResponsesRequest) (int, error) {
	var choice struct {
		Type  string `json:"type"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
	}
	if common.GetJsonType(request.ToolChoice) == "object" {
		if err := common.Unmarshal(request.ToolChoice, &choice); err != nil {
			return 0, fmt.Errorf("invalid Responses tool_choice: %w", err)
		}
		if isResponsesWebSearchType(choice.Type) {
			return 0, fmt.Errorf("built-in web search is disabled for this model; a web-search tool_choice cannot be honored")
		}
		for _, tool := range choice.Tools {
			if isResponsesWebSearchType(tool.Type) {
				return 0, fmt.Errorf("built-in web search is disabled for this model; remove it from tool_choice.tools")
			}
		}
	}
	if len(request.Tools) == 0 {
		return 0, nil
	}
	var tools []json.RawMessage
	if err := common.Unmarshal(request.Tools, &tools); err != nil {
		return 0, fmt.Errorf("invalid Responses tools: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(tools))
	removed := 0
	for _, raw := range tools {
		var tool struct {
			Type string `json:"type"`
		}
		if err := common.Unmarshal(raw, &tool); err != nil {
			return 0, fmt.Errorf("invalid Responses tool: %w", err)
		}
		if isResponsesWebSearchType(tool.Type) {
			removed++
			continue
		}
		kept = append(kept, raw)
	}
	if removed == 0 {
		return 0, nil
	}
	if len(kept) == 0 && common.GetJsonType(request.ToolChoice) == "string" {
		var mode string
		if err := common.Unmarshal(request.ToolChoice, &mode); err != nil {
			return 0, fmt.Errorf("invalid Responses tool_choice: %w", err)
		}
		if mode == "required" {
			return 0, fmt.Errorf("built-in web search is disabled for this model; tool_choice=required has no remaining tools")
		}
	}
	encoded, err := common.Marshal(kept)
	if err != nil {
		return 0, fmt.Errorf("failed to encode Responses tools: %w", err)
	}
	request.Tools = encoded
	return removed, nil
}

func isResponsesWebSearchType(toolType string) bool {
	switch strings.TrimSpace(toolType) {
	case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
		return true
	default:
		return false
	}
}
