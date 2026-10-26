package relay

import (
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

// budgetArgonResponsesImages preserves all non-tool images and spends the
// remaining ten-image budget on the newest tool images. Only the outbound copy
// is changed. Raw fields preserve unknown metadata and numeric precision.
func budgetArgonResponsesImages(request *dto.OpenAIResponsesRequest) (int, error) {
	if common.GetJsonType(request.Input) != "array" {
		return 0, nil
	}
	var items []map[string]json.RawMessage
	if err := common.Unmarshal(request.Input, &items); err != nil {
		return 0, fmt.Errorf("invalid Responses image-budget input: %w", err)
	}
	type imageGroup struct {
		item   int
		field  string
		tool   bool
		parts  []json.RawMessage
		images []int
	}
	var groups []imageGroup
	protected, toolImages := 0, 0
	for i, item := range items {
		var kind string
		if raw := item["type"]; len(raw) > 0 {
			if err := common.Unmarshal(raw, &kind); err != nil {
				return 0, fmt.Errorf("invalid Responses input item type: %w", err)
			}
		}
		field, tool := "content", false
		switch kind {
		case "function_call_output", "custom_tool_call_output":
			field, tool = "output", true
		case "", "message":
		default:
			continue
		}
		if common.GetJsonType(item[field]) != "array" {
			continue
		}
		var parts []json.RawMessage
		if err := common.Unmarshal(item[field], &parts); err != nil {
			return 0, fmt.Errorf("invalid Responses image-budget content: %w", err)
		}
		var images []int
		for j, part := range parts {
			if common.GetJsonType(part) != "object" {
				continue
			}
			var header struct {
				Type string `json:"type"`
			}
			if err := common.Unmarshal(part, &header); err != nil {
				return 0, fmt.Errorf("invalid Responses image-budget part: %w", err)
			}
			if header.Type != "input_image" {
				continue
			}
			images = append(images, j)
			if tool {
				toolImages++
			} else {
				protected++
			}
		}
		groups = append(groups, imageGroup{item: i, field: field, tool: tool, parts: parts, images: images})
	}
	if protected > 10 {
		return 0, fmt.Errorf("Argon supports at most 10 images per request, but this conversation contains %d non-tool images; user attachments were not removed. Reduce attached images or use a model/provider with a higher image limit", protected)
	}
	remove := protected + toolImages - 10
	if remove <= 0 {
		return 0, nil
	}
	omitted := 0
	for _, group := range groups {
		if !group.tool {
			continue
		}
		changed := false
		for _, i := range group.images {
			if omitted == remove {
				break
			}
			marker, err := common.Marshal(map[string]string{"type": "input_text", "text": "[Argon image budget: an older tool image was omitted from this request to stay within the 10-image limit. The original remains in the chat history. Its pixels are unavailable in this request; do not infer unseen details. Re-open the source if needed.]"})
			if err != nil {
				return 0, err
			}
			group.parts[i] = marker
			omitted++
			changed = true
		}
		if changed {
			encoded, err := common.Marshal(group.parts)
			if err != nil {
				return 0, fmt.Errorf("failed to encode budgeted tool images: %w", err)
			}
			items[group.item][group.field] = encoded
		}
	}
	encoded, err := common.Marshal(items)
	if err != nil {
		return 0, fmt.Errorf("failed to encode budgeted Responses input: %w", err)
	}
	request.Input = encoded
	return omitted, nil
}
