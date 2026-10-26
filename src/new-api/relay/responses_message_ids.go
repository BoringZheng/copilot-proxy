package relay

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
)

var legacyResponsesMessageID = regexp.MustCompile(`^(chatcmpl-|resp_)[A-Za-z0-9_-]+_msg_[0-9]+$`)
var legacyResponsesToolID = regexp.MustCompile(`^(call_[A-Za-z0-9_-]+|(chatcmpl-|resp_)[A-Za-z0-9_-]+_call_[0-9]+)$`)

// normalizeLegacyResponsesMessageIDs removes only optional synthetic IDs on
// inline assistant messages and legacy tool calls sent to the configured origin.
// It does not invent server-side identities or touch native/opaque IDs, item
// references, correlation call_id values, encrypted reasoning, or client history.
func normalizeLegacyResponsesMessageIDs(request *dto.OpenAIResponsesRequest, baseURL string) (bool, error) {
	origin := os.Getenv("COPILOT_RESPONSES_LEGACY_MESSAGE_ID_ORIGIN")
	if origin == "" || common.GetJsonType(request.Input) != "array" {
		return false, nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || !common.URLOriginInList(parsed, origin) {
		return false, nil
	}
	var items []json.RawMessage
	if err := common.Unmarshal(request.Input, &items); err != nil {
		return false, fmt.Errorf("invalid Responses message history: %w", err)
	}
	changed := false
	for i, raw := range items {
		if common.GetJsonType(raw) != "object" {
			continue
		}
		var header struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			ID      string          `json:"id"`
			CallID  string          `json:"call_id"`
			Content json.RawMessage `json:"content"`
		}
		if err := common.Unmarshal(raw, &header); err != nil {
			// This compatibility policy is not a validator for unrelated items.
			continue
		}
		legacyMessage := (header.Type == "" || header.Type == "message") && header.Role == "assistant" && len(header.Content) > 0 && string(header.Content) != "null" && legacyResponsesMessageID.MatchString(header.ID)
		// The old converter reused call_id as the optional item id. Remove only
		// that recognizable duplicate, never a native or unrelated tool item ID.
		legacyTool := (header.Type == "function_call" || header.Type == "custom_tool_call") && header.ID == header.CallID && legacyResponsesToolID.MatchString(header.ID)
		if !legacyMessage && !legacyTool {
			continue
		}
		var item map[string]json.RawMessage
		if err := common.Unmarshal(raw, &item); err != nil {
			return false, fmt.Errorf("invalid Responses message history item: %w", err)
		}
		delete(item, "id")
		items[i], err = common.Marshal(item)
		if err != nil {
			return false, err
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	request.Input, err = common.Marshal(items)
	return true, err
}
