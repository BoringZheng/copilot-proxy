package relay

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareResponsesRequestImageBudget(t *testing.T) {
	for _, tc := range []struct {
		name, model, enabled                   string
		user, tool                             int
		custom, passthrough, native, wantError bool
		wantImages, wantOmitted                int
	}{
		{name: "eleven tool images", model: "gemini-4-argon", enabled: "10", tool: 11, wantImages: 10, wantOmitted: 1},
		{name: "twelve custom tool images", model: "gemini-4-argon", enabled: "10", tool: 12, custom: true, wantImages: 10, wantOmitted: 2},
		{name: "attachments take priority even after tools", model: "gemini-4-argon", enabled: "10", user: 2, tool: 11, wantImages: 10, wantOmitted: 3},
		{name: "ten attachments displace tool images", model: "gemini-4-argon", enabled: "10", user: 10, tool: 2, wantImages: 10, wantOmitted: 2},
		{name: "attachments never silently removed", model: "gemini-4-argon", enabled: "10", user: 11, tool: 2, wantError: true},
		{name: "exactly ten unchanged", model: "gemini-4-argon", enabled: "10", user: 1, tool: 9, wantImages: 10},
		{name: "disabled unchanged", model: "gemini-4-argon", tool: 12, wantImages: 12},
		{name: "GPT unchanged", model: "gpt-6.1-sol", enabled: "10", tool: 12, native: true, wantImages: 12},
		{name: "DeepSeek unchanged", model: "deepseek-flash", enabled: "10", tool: 12, native: true, wantImages: 12},
		{name: "exact model match", model: "gemini-4-argon-extra", enabled: "10", tool: 12, native: true, wantImages: 12},
		{name: "passthrough rejected", model: "gemini-4-argon", enabled: "10", tool: 11, passthrough: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ARGON_RESPONSES_IMAGE_BUDGET", tc.enabled)
			t.Setenv("DISABLE_RESPONSES_WEB_SEARCH_MODELS", "")
			var input []map[string]any
			for i := range tc.tool {
				kind, result, field := "function_call", "function_call_output", "arguments"
				if tc.custom {
					kind, result, field = "custom_tool_call", "custom_tool_call_output", "input"
				}
				id := fmt.Sprintf("call_%d", i)
				input = append(input, map[string]any{"type": kind, "call_id": id, "name": "inspect", field: "{}"}, map[string]any{"type": result, "call_id": id, "output": []any{
					map[string]any{"type": "input_text", "text": fmt.Sprintf("tool text %d", i)},
					map[string]any{"type": "input_image", "image_url": fmt.Sprintf("https://example.test/tool-%d.png", i)},
				}})
			}
			content := []any{map[string]any{"type": "input_text", "text": "continue"}}
			for i := range tc.user {
				content = append(content, map[string]any{"type": "input_image", "image_url": fmt.Sprintf("https://example.test/user-%d.png", i)})
			}
			input = append(input, map[string]any{"role": "user", "content": content})
			raw, err := common.Marshal(input)
			require.NoError(t, err)
			req := &dto.OpenAIResponsesRequest{Model: tc.model, Input: raw}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, tc.model)
			channel := constant.ChannelTypeAdvancedCustom
			if tc.native {
				channel = constant.ChannelTypeOpenAI
			}
			common.SetContextKey(c, constant.ContextKeyChannelType, channel)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "http://127.0.0.1:1")
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: tc.passthrough})
			common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{ToolLossPolicy: "safe", AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", Converter: relayconvert.ConverterOpenAIResponsesToOpenAIChat}}}})
			_, body, closer, apiErr := PrepareResponsesRequest(c, relaycommon.GenRelayInfoResponses(c, req), req)
			assert.Equal(t, string(raw), string(req.Input), "original history remains intact")
			if tc.wantError {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.True(t, types.IsSkipRetryError(apiErr))
				return
			}
			require.Nil(t, apiErr)
			defer closer.Close()
			encoded, err := io.ReadAll(body)
			require.NoError(t, err)
			text := string(encoded)
			assert.Equal(t, tc.wantImages, strings.Count(text, "https://example.test/"))
			assert.Equal(t, tc.wantOmitted, strings.Count(text, "Argon image budget:"))
			for i := range tc.user {
				assert.Contains(t, text, fmt.Sprintf("https://example.test/user-%d.png", i))
			}
			for i := range tc.tool {
				assert.Contains(t, text, fmt.Sprintf("tool text %d", i))
				assert.Contains(t, text, fmt.Sprintf("call_%d", i))
				url := fmt.Sprintf("https://example.test/tool-%d.png", i)
				if i < tc.wantOmitted {
					assert.NotContains(t, text, url)
				} else {
					assert.Contains(t, text, url)
				}
			}
		})
	}
}

func TestArgonImageBudgetPreservesRawContent(t *testing.T) {
	for _, input := range []string{`"hello"`, `[{"type":"function_call_output","call_id":"c","output":[1,"x",{"number":9007199254740993}]}]`, `[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/a"}],"extra":9007199254740993}]`} {
		req := &dto.OpenAIResponsesRequest{Input: []byte(input)}
		n, err := budgetArgonResponsesImages(req)
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.Equal(t, input, string(req.Input))
	}
	input := `[{"type":"custom_tool_call_output","call_id":"c","extra":9007199254740993,"output":[{"type":"input\u005fimage","image_url":"old"}` + strings.Repeat(`,{"type":"input_image","image_url":"new"}`, 10) + `]}]`
	req := &dto.OpenAIResponsesRequest{Input: []byte(input)}
	n, err := budgetArgonResponsesImages(req)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, string(req.Input), `9007199254740993`)
	assert.NotContains(t, string(req.Input), `"old"`)
	assert.Contains(t, string(req.Input), `"call_id":"c"`)
	n, err = budgetArgonResponsesImages(req)
	require.NoError(t, err)
	assert.Zero(t, n, "retry is idempotent")
}
