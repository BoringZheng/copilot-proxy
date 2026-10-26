package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestPrepareResponsesRequestLegacyMessageIDs(t *testing.T) {
	const origin = "http://copilot.invalid:9876"
	const history = `[
	 {"type":"message","role":"assistant","id":"chatcmpl-202610180622442811935288268d9d6LoODbaJY_msg_0","content":[{"type":"output_text","text":"Keep chatcmpl-original_msg_0 verbatim","annotations":[]}],"extra":{"id":"nested-id","large":9007199254740993,"zero":0,"off":false}},
	 {"role":"assistant","id":"resp_old_msg_1","content":"second"},
	 {"type":"message","role":"assistant","id":"msg_native","content":[]},
	 {"type":"message","role":"user","id":"chatcmpl-user_msg_0","content":[]},
	 {"type":"reasoning","id":"chatcmpl-reasoning_msg_0","encrypted_content":"opaque-original","summary":[]},
	 {"type":"function_call","id":"chatcmpl-tool_msg_0","call_id":"call-original","name":"inspect","arguments":"{}"},
	 {"type":"function_call_output","call_id":"call-original","output":"keep output"},
	 {"type":"custom_tool_call","id":"chatcmpl-custom_msg_0","call_id":"custom-original","name":"exec","input":"keep input"},
	 {"type":"custom_tool_call_output","call_id":"custom-original","output":"keep custom output"},
	 {"type":"item_reference","id":"chatcmpl-reference_msg_0"},
	 {"type":"message","role":"assistant","id":"opaque_msg_0","content":[]},
	 {"type":"custom_tool_call","id":"call_MHxUd3luSkQ0b0F6amI3ZG1uaW8","call_id":"call_MHxUd3luSkQ0b0F6amI3ZG1uaW8","name":"exec","input":"verbatim call_MHxUd3luSkQ0b0F6amI3ZG1uaW8"},
	 {"type":"custom_tool_call_output","id":"ctco_native","call_id":"call_MHxUd3luSkQ0b0F6amI3ZG1uaW8","output":"unchanged"},
	 {"type":"function_call","id":"call_old","call_id":"call_old","name":"inspect","arguments":"{\"zero\":0,\"off\":false,\"large\":9007199254740993}"},
	 {"type":"function_call_output","call_id":"call_old","output":"unchanged"},
	 {"type":"function_call","id":"chatcmpl-old_call_0","call_id":"chatcmpl-old_call_0","name":"inspect","arguments":"{}"},
	 {"type":"function_call","id":"fc_native","call_id":"fc_native","name":"inspect","arguments":"{}"},
	 {"type":"custom_tool_call","id":"call_unrelated","call_id":"call_original","name":"exec","input":"unchanged"},
	 {"type":"item_reference","id":"call_old"}
	]`
	for _, tc := range []struct {
		name, configured, base string
		passthrough, changed   bool
	}{
		{name: "native Copilot", configured: origin, base: origin + "/v1", changed: true},
		{name: "primary in list", configured: origin + ",http://copilot.invalid:9877", base: origin + "/v1", changed: true},
		{name: "secondary in list", configured: origin + ", http://copilot.invalid:9877 ,", base: "http://copilot.invalid:9877/v1", changed: true},
		{name: "unrelated to list", configured: origin + ",http://copilot.invalid:9877", base: "http://deepseek.invalid/v1"},
		{name: "list invalid entries", configured: ",://invalid," + origin + "/path," + origin + "0", base: origin},
		{name: "invalid target", configured: origin, base: "://invalid"},
		{name: "disabled", base: origin},
		{name: "different origin", configured: origin, base: "http://deepseek.invalid"},
		{name: "not prefix match", configured: origin, base: "http://copilot.invalid:98760"},
		{name: "passthrough explicit error", configured: origin, base: origin, passthrough: true, changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COPILOT_RESPONSES_LEGACY_MESSAGE_ID_ORIGIN", tc.configured)
			t.Setenv("DISABLE_RESPONSES_WEB_SEARCH_MODELS", "")
			t.Setenv("ARGON_RESPONSES_IMAGE_BUDGET", "")
			req := &dto.OpenAIResponsesRequest{Model: "gpt-test", Input: []byte(history)}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, req.Model)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, tc.base)
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: tc.passthrough})
			_, body, closer, apiErr := PrepareResponsesRequest(c, relaycommon.GenRelayInfoResponses(c, req), req)
			assert.Equal(t, history, string(req.Input), "the saved/incoming history must not change")
			if tc.passthrough {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.Nil(t, body)
				return
			}
			require.Nil(t, apiErr)
			defer closer.Close()
			encoded, err := io.ReadAll(body)
			require.NoError(t, err)
			var target dto.OpenAIResponsesRequest
			require.NoError(t, common.Unmarshal(encoded, &target))
			var want []map[string]json.RawMessage
			require.NoError(t, common.Unmarshal([]byte(history), &want))
			if tc.changed {
				delete(want[0], "id")
				delete(want[1], "id")
				delete(want[11], "id")
				delete(want[13], "id")
				delete(want[15], "id")
			}
			expected, err := common.Marshal(want)
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(target.Input))
			assert.Contains(t, string(target.Input), "9007199254740993")
			assert.Contains(t, string(target.Input), `"off":false`)
			assert.Contains(t, string(target.Input), `"zero":0`)
		})
	}
}

func TestPrepareResponsesRequestWebSearchPolicy(t *testing.T) {
	const function = `{"type":"function","name":"web_search","description":"client search function","parameters":{"type":"object","properties":{"type":{"const":"web_search"}}}}`
	for _, tc := range []struct {
		name, model, disabledModels, tools, choice, input string
		wantError, passthrough, native                    bool
		wantFunctions                                     int
	}{
		{name: "default unchanged under safe", model: "gemini-4-argon", tools: `[{"type":"web_search","external_web_access":true}]`, wantError: true},
		{name: "live search removed before conversion", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search","external_web_access":true},` + function + `]`, wantFunctions: 1},
		{name: "cached search removed entirely", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search","external_web_access":false},` + function + `]`, wantFunctions: 1},
		{name: "custom exec and namespace preserved", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search","external_web_access":true},{"type":"custom","name":"exec","description":"Execute JavaScript"},{"type":"namespace","name":"helpers","tools":[{"type":"function","name":"inspect","parameters":{"type":"object","properties":{}}}]}]`, wantFunctions: 2},
		{name: "all known search variants", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search_preview"},{"type":"web_search_preview_2025_03_11"},{"type":"web_search_2025_08_26"},` + function + `]`, wantFunctions: 1},
		{name: "non Argon remains rejected not silently changed", model: "gpt-6.1-sol", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search","external_web_access":true}]`, wantError: true},
		{name: "native GPT keeps search", model: "gpt-6.1-sol", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search","external_web_access":true}]`, native: true},
		{name: "no search and exact model list", model: "gemini-4-argon", disabledModels: " other, gemini-4-argon ", tools: `[` + function + `]`, wantFunctions: 1},
		{name: "model prefix is not a match", model: "gemini-4-argon-extra", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search","external_web_access":true}]`, wantError: true},
		{name: "forced hosted search rejected", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search"}]`, choice: `{"type":"web_search"}`, wantError: true},
		{name: "restricted hosted search rejected", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search"}]`, choice: `{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}`, wantError: true},
		{name: "required with no remaining tools rejected", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search"}]`, choice: `"required"`, wantError: true},
		{name: "forced ordinary search function preserved", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search"},` + function + `]`, choice: `{"type":"function","name":"web_search"}`, wantFunctions: 1},
		{name: "hosted history not deleted to bypass safe", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search"}]`, input: `[{"type":"web_search_call","id":"ws_test","status":"completed","action":{"type":"search","query":"synthetic"}}]`, wantError: true},
		{name: "passthrough cannot bypass explicit disable", model: "gemini-4-argon", disabledModels: "gemini-4-argon", tools: `[{"type":"web_search"}]`, passthrough: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DISABLE_RESPONSES_WEB_SEARCH_MODELS", tc.disabledModels)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, tc.model)
			channelType := constant.ChannelTypeAdvancedCustom
			if tc.native {
				channelType = constant.ChannelTypeOpenAI
			}
			common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "http://127.0.0.1:1")
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: tc.passthrough})
			common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
				ToolLossPolicy: "safe",
				AdvancedCustom: &dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{IncomingPath: "/v1/responses", UpstreamPath: "/v1/chat/completions", Converter: relayconvert.ConverterOpenAIResponsesToOpenAIChat}}},
			})
			input := tc.input
			if input == "" {
				input = `"synthetic diagnostic"`
			}
			request := &dto.OpenAIResponsesRequest{Model: tc.model, Input: []byte(input), Tools: []byte(tc.tools), ToolChoice: []byte(tc.choice)}
			original, err := common.Marshal(request)
			require.NoError(t, err)
			info := relaycommon.GenRelayInfoResponses(c, request)
			_, body, closer, apiErr := PrepareResponsesRequest(c, info, request)
			if tc.wantError {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
				assert.True(t, types.IsSkipRetryError(apiErr))
				assert.Nil(t, body)
			} else {
				require.Nil(t, apiErr)
				require.NotNil(t, closer)
				defer closer.Close()
				encoded, err := io.ReadAll(body)
				require.NoError(t, err)
				if tc.native {
					var target dto.OpenAIResponsesRequest
					require.NoError(t, common.Unmarshal(encoded, &target))
					assert.JSONEq(t, tc.tools, string(target.Tools))
				} else {
					var target dto.GeneralOpenAIRequest
					require.NoError(t, common.Unmarshal(encoded, &target))
					assert.Len(t, target.Tools, tc.wantFunctions)
					assert.Nil(t, target.WebSearchOptions)
					if tc.wantFunctions == 1 {
						assert.Equal(t, "web_search", target.Tools[0].Function.Name)
					}
				}
			}
			unchanged, err := common.Marshal(request)
			require.NoError(t, err)
			assert.Equal(t, string(original), string(unchanged), "incoming request stays intact for retries and audit")
		})
	}
}
