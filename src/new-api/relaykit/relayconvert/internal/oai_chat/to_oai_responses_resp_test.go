package oaichat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatToResponsesToolItemIDs(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, callID := range []string{"call_MHxUd3luSkQ0b0F6amI3ZG1uaW8", ""} {
			t.Run(fmt.Sprintf("custom=%t/missing=%t", custom, callID == ""), func(t *testing.T) {
				responseID := "chatcmpl-" + strings.Repeat("long", 40)
				name, arguments, prefix := "inspect", `{"zero":0,"off":false}`, "fc"
				var tools *convmeta.ResponsesToolState
				if custom {
					name, arguments, prefix = "exec", `{"input":"verbatim input"}`, "ctc"
					tools = execCustomToolState()
				}
				message := dto.Message{Role: "assistant"}
				message.SetToolCalls([]dto.ToolCallRequest{{ID: callID, Type: "function", Function: dto.FunctionRequest{Name: name, Arguments: arguments}}})
				response, _, err := ChatCompletionsResponseToResponsesResponseWithTools(&dto.OpenAITextResponse{
					Choices: []dto.OpenAITextResponseChoice{{Message: message, FinishReason: "tool_calls"}},
				}, responseID, tools)
				require.NoError(t, err)
				require.Len(t, response.Output, 1)
				want := response.Output[0]
				assert.Regexp(t, "^"+prefix+"_[0-9a-f]{32}$", want.ID)
				assert.NotEqual(t, want.CallId, want.ID)
				if callID != "" {
					assert.Equal(t, callID, want.CallId)
				} else {
					assert.Equal(t, responseID+"_call_0", want.CallId)
				}
				state := NewChatToResponsesStreamState(responseID, "test")
				state.Tools = tools
				index := 0
				// The name arrives before the ID; no premature correlation ID
				// may escape, and held arguments must be emitted exactly once.
				events := mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
					Choices: []dto.ChatCompletionsStreamResponseChoice{{Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{{Index: &index, Function: dto.FunctionResponse{Name: name, Arguments: arguments}}}}}},
				})
				for _, event := range events {
					assert.NotEqual(t, responsesEventOutputItemAdded, event.Type)
				}
				if callID != "" {
					events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
						Choices: []dto.ChatCompletionsStreamResponseChoice{{Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{{Index: &index, ID: callID}}}}},
					})...)
				}
				events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)
				assert.Equal(t, arguments, state.UsageText(), "held arguments contribute exactly once, even if the provider omits its ID")
				for _, event := range events {
					if event.Payload.ItemID != "" {
						assert.Equal(t, want.ID, event.Payload.ItemID)
					}
					if item := event.Payload.Item; item != nil {
						assert.Equal(t, want.ID, item.ID)
						assert.Equal(t, want.CallId, item.CallId)
					}
				}
				output := events[len(events)-1].Payload.Response.Output
				require.Len(t, output, 1)
				assert.Equal(t, want, output[0])
			})
		}
	}
}

func TestChatToResponsesMessageIDs(t *testing.T) {
	for _, id := range []string{"chatcmpl-202610180622442811935288268d9d6LoODbaJY", "resp_1", "chatcmpl-" + strings.Repeat("long/你好", 30)} {
		t.Run(id[:min(len(id), 30)], func(t *testing.T) {
			chat := &dto.OpenAITextResponse{Choices: []dto.OpenAITextResponseChoice{{Message: dto.Message{Role: "assistant", Content: "hello"}}}}
			response, _, err := ChatCompletionsResponseToResponsesResponse(chat, id)
			require.NoError(t, err)
			require.Len(t, response.Output, 1)
			messageID := response.Output[0].ID
			assert.Regexp(t, `^msg_[0-9a-f]{32}$`, messageID)
			assert.Equal(t, "hello", response.Output[0].Content[0].Text)
			state := NewChatToResponsesStreamState(id, "gpt-test")
			events := mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
				Choices: []dto.ChatCompletionsStreamResponseChoice{{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Content: lo.ToPtr("hello")}}},
			})
			events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)
			for _, event := range events {
				if event.Payload.Item != nil && event.Payload.Item.Type == responsesOutputTypeMessage {
					assert.Equal(t, messageID, event.Payload.Item.ID)
				}
				if event.Payload.ItemID != "" {
					assert.Equal(t, messageID, event.Payload.ItemID)
				}
			}
			last := events[len(events)-1]
			require.NotNil(t, last.Payload.Response)
			assert.Equal(t, messageID, last.Payload.Response.Output[0].ID)
		})
	}
}

func TestChatCompletionsResponseToResponsesPreservesTextToolCallsAndUsage(t *testing.T) {
	chat := &dto.OpenAITextResponse{
		Id:      "chatcmpl_1",
		Model:   "gpt-test",
		Created: 456,
		Choices: []dto.OpenAITextResponseChoice{
			{
				Message:      assistantMessageWithTool("I will call.", "call_1", "lookup", `{"q":"x"}`),
				FinishReason: "tool_calls",
			},
		},
		Usage: dto.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
	}

	resp, usage, err := ChatCompletionsResponseToResponsesResponse(chat, "resp_1")
	require.NoError(t, err)
	require.NotNil(t, usage)

	assert.Equal(t, "resp_1", resp.ID)
	assert.Equal(t, "response", resp.Object)
	assert.Equal(t, `"completed"`, string(resp.Status))
	assert.Equal(t, 3, resp.Usage.InputTokens)
	assert.Equal(t, 5, resp.Usage.OutputTokens)
	require.Len(t, resp.Output, 2)
	assert.Equal(t, responsesOutputTypeMessage, resp.Output[0].Type)
	assert.Equal(t, "I will call.", resp.Output[0].Content[0].Text)
	assert.Equal(t, responsesOutputTypeFunctionCall, resp.Output[1].Type)
	assert.Equal(t, "call_1", resp.Output[1].CallId)
	assert.Equal(t, "lookup", resp.Output[1].Name)
	assert.Equal(t, `"{\"q\":\"x\"}"`, string(resp.Output[1].Arguments))
}

func TestChatCompletionsResponseToResponsesDropsEmptyNameToolCalls(t *testing.T) {
	usage := dto.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}
	usage.PromptTokensDetails.CachedTokens = 7
	usage.BillingUsage = dto.NewOpenAIChatBillingUsage(&usage)
	message := dto.Message{Role: "assistant"}
	message.SetToolCalls([]dto.ToolCallRequest{
		{
			ID:   "call_invalid",
			Type: "function",
			Function: dto.FunctionRequest{
				Arguments: `{"ok":true}`,
			},
		},
		{
			ID:       "call_whitespace",
			Function: dto.FunctionRequest{Name: " \t\n", Arguments: `{}`},
		},
		{
			ID:   "call_valid",
			Type: "function",
			Function: dto.FunctionRequest{
				Name:      "lookup",
				Arguments: `{"q":"x"}`,
			},
		},
	})

	resp, convertedUsage, err := ChatCompletionsResponseToResponsesResponse(&dto.OpenAITextResponse{
		Usage: usage,
		Choices: []dto.OpenAITextResponseChoice{{
			Message:      message,
			FinishReason: "tool_calls",
		}},
	}, "resp_1")
	require.NoError(t, err)

	require.Len(t, resp.Output, 1)
	assert.Equal(t, "call_valid", resp.Output[0].CallId)
	assert.Equal(t, "lookup", resp.Output[0].Name)
	assert.Equal(t, `"{\"q\":\"x\"}"`, string(resp.Output[0].Arguments))
	assert.Equal(t, 20, convertedUsage.InputTokens)
	assert.Equal(t, 5, convertedUsage.OutputTokens)
	assert.Equal(t, 25, convertedUsage.TotalTokens)
	require.NotNil(t, convertedUsage.InputTokensDetails)
	assert.Equal(t, 7, convertedUsage.InputTokensDetails.CachedTokens)
	assert.Equal(t, usage.BillingUsage, convertedUsage.BillingUsage)
}

func TestChatCompletionsResponseToResponsesEmitsReasoningSummaryBeforeText(t *testing.T) {
	message := dto.Message{Role: "assistant", Content: "final answer"}
	message.ReasoningContent = lo.ToPtr("thinking summary")
	resp, _, err := ChatCompletionsResponseToResponsesResponse(&dto.OpenAITextResponse{
		Id:    "chatcmpl_1",
		Model: "gpt-test",
		Choices: []dto.OpenAITextResponseChoice{
			{Message: message, FinishReason: "stop"},
		},
	}, "resp_1")
	require.NoError(t, err)

	require.Len(t, resp.Output, 2)
	assert.Equal(t, responsesOutputTypeReasoning, resp.Output[0].Type)
	require.Len(t, resp.Output[0].Summary, 1)
	assert.Equal(t, "thinking summary", resp.Output[0].Summary[0].Text)
	assert.Empty(t, resp.Output[0].Content)
	assert.Equal(t, responsesOutputTypeMessage, resp.Output[1].Type)
	assert.Equal(t, "final answer", resp.Output[1].Content[0].Text)
}

func TestChatCompletionsResponseToResponsesMapsIncompleteFinishReasons(t *testing.T) {
	tests := []struct {
		name         string
		finishReason string
		wantReason   string
	}{
		{name: "length", finishReason: "length", wantReason: responsesIncompleteReasonMaxTokens},
		{name: "content filter", finishReason: "content_filter", wantReason: responsesIncompleteReasonContentFilter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _, err := ChatCompletionsResponseToResponsesResponse(&dto.OpenAITextResponse{
				Id:    "chatcmpl_1",
				Model: "gpt-test",
				Choices: []dto.OpenAITextResponseChoice{
					{
						Message:      dto.Message{Role: "assistant", Content: "partial"},
						FinishReason: tt.finishReason,
					},
				},
			}, "resp_1")
			require.NoError(t, err)

			assert.Equal(t, `"incomplete"`, string(resp.Status))
			require.NotNil(t, resp.IncompleteDetails)
			assert.Equal(t, tt.wantReason, resp.IncompleteDetails.Reason)
			require.Len(t, resp.Output, 1)
			assert.Equal(t, "incomplete", resp.Output[0].Status)
		})
	}
}

func TestChatCompletionsStreamToResponsesEventsAggregatesUsageAndToolArgs(t *testing.T) {
	state := NewChatToResponsesStreamState("resp_1", "gpt-test")
	state.Created = 123
	toolIndex := 0

	var events []ChatToResponsesStreamEvent
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Id:      "chatcmpl_1",
		Model:   "gpt-test",
		Created: 123,
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Role: "assistant"}},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Content: lo.ToPtr("hello")}},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{
				{Index: &toolIndex, ID: "call_1", Type: "function", Function: dto.FunctionResponse{Name: "lookup"}},
			}}},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{
				{Index: &toolIndex, Function: dto.FunctionResponse{Arguments: `{"q":"x"}`}},
			}}},
		},
	})...)
	finishReason := "tool_calls"
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{
			{Index: 0, FinishReason: &finishReason},
		},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Usage: &dto.Usage{PromptTokens: 2, CompletionTokens: 4, TotalTokens: 6},
	})...)
	events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

	require.Len(t, events, 10)
	assert.Equal(t, responsesEventCreated, events[0].Type)
	assert.Equal(t, responsesEventOutputTextDelta, events[2].Type)
	assert.Equal(t, "hello", events[2].Payload.Delta)
	assert.Equal(t, responsesEventFunctionArgsDelta, events[4].Type)
	assert.Equal(t, `{"q":"x"}`, events[4].Payload.Delta)
	assert.Equal(t, responsesEventCompleted, events[9].Type)
	require.NotNil(t, events[9].Payload.Response)
	assert.Equal(t, 6, events[9].Payload.Response.Usage.TotalTokens)
	require.Len(t, events[9].Payload.Response.Output, 2)
	assert.Equal(t, "hello", events[9].Payload.Response.Output[0].Content[0].Text)
	assert.Equal(t, `"{\"q\":\"x\"}"`, string(events[9].Payload.Response.Output[1].Arguments))
}

func TestChatCompletionsStreamToResponsesEmitsReasoningSummaryPartLifecycle(t *testing.T) {
	state := NewChatToResponsesStreamState("resp_1", "gpt-test")
	stop := "stop"

	var events []ChatToResponsesStreamEvent
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Id: "chatcmpl_1", Model: "gpt-test",
		Choices: []dto.ChatCompletionsStreamResponseChoice{{
			Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ReasoningContent: lo.ToPtr("think")},
		}},
	})...)
	events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
		Choices: []dto.ChatCompletionsStreamResponseChoice{{
			Delta:        dto.ChatCompletionsStreamResponseChoiceDelta{Content: lo.ToPtr("answer")},
			FinishReason: &stop,
		}},
	})...)
	events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	assert.Equal(t, []string{
		responsesEventCreated,
		responsesEventOutputItemAdded,
		responsesEventReasoningSummaryPartAdded,
		responsesEventReasoningSummaryDelta,
		responsesEventOutputItemAdded,
		responsesEventOutputTextDelta,
		"response.output_text.done",
		responsesEventOutputItemDone,
		responsesEventReasoningSummaryDone,
		responsesEventReasoningSummaryPartDone,
		responsesEventOutputItemDone,
		responsesEventCompleted,
	}, types)

	partAdded := events[2].Payload
	assert.Equal(t, responsesItemID("rs", "resp_1", 0), partAdded.ItemID)
	assert.Equal(t, 0, lo.FromPtr(partAdded.OutputIndex))
	assert.Equal(t, 0, lo.FromPtr(partAdded.SummaryIndex))
	require.NotNil(t, partAdded.Part)
	assert.Equal(t, dto.ResponsesReasoningSummaryPart{Type: "summary_text"}, *partAdded.Part)

	partDone := events[9].Payload
	assert.Equal(t, responsesItemID("rs", "resp_1", 0), partDone.ItemID)
	require.NotNil(t, partDone.Part)
	assert.Equal(t, dto.ResponsesReasoningSummaryPart{Type: "summary_text", Text: "think"}, *partDone.Part)
}

func TestChatCompletionsStreamToResponsesReopensItemsAfterMidStreamFinishReason(t *testing.T) {
	for _, emitSequenceNumber := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "sequence_numbers"}[emitSequenceNumber], func(t *testing.T) {
			state := NewChatToResponsesStreamState("resp_1", "gpt-test")
			state.EmitSequenceNumber = emitSequenceNumber
			toolIndex := 0
			toolCalls := "tool_calls"
			stop := "stop"

			chunks := []*dto.ChatCompletionsStreamResponse{
				{Id: "chatcmpl_1", Model: "gpt-test", Choices: []dto.ChatCompletionsStreamResponseChoice{{
					Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ReasoningContent: lo.ToPtr("round 1")},
				}}},
				{Choices: []dto.ChatCompletionsStreamResponseChoice{{
					Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Content: lo.ToPtr("calling")},
				}}},
				{Choices: []dto.ChatCompletionsStreamResponseChoice{{
					Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{{
						Index: &toolIndex, ID: "call_1", Type: "function",
						Function: dto.FunctionResponse{Name: "lookup", Arguments: "{}"},
					}}},
				}}},
				{Choices: []dto.ChatCompletionsStreamResponseChoice{{FinishReason: &toolCalls}}},
				{Choices: []dto.ChatCompletionsStreamResponseChoice{{
					Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ReasoningContent: lo.ToPtr("round 2")},
				}}},
				{Choices: []dto.ChatCompletionsStreamResponseChoice{{
					Delta:        dto.ChatCompletionsStreamResponseChoiceDelta{Content: lo.ToPtr("final")},
					FinishReason: &stop,
				}}},
			}
			var events []ChatToResponsesStreamEvent
			for _, chunk := range chunks {
				events = append(events, mustResponsesEventsFromChatChunk(t, state, chunk)...)
			}
			events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

			open := map[string]bool{}
			partAdded, partDone := 0, 0
			for _, event := range events {
				itemID := event.Payload.ItemID
				if event.Payload.Item != nil {
					itemID = event.Payload.Item.ID
				}
				switch event.Type {
				case responsesEventOutputItemAdded:
					assert.Falsef(t, open[itemID], "item %q added twice", itemID)
					open[itemID] = true
				case responsesEventOutputItemDone:
					assert.Truef(t, open[itemID], "item %q done without being open", itemID)
					delete(open, itemID)
				case responsesEventReasoningSummaryPartAdded:
					partAdded++
					assert.Truef(t, open[itemID], "part added for closed item %q", itemID)
				case responsesEventReasoningSummaryPartDone:
					partDone++
					assert.Truef(t, open[itemID], "part done for closed item %q", itemID)
				case responsesEventReasoningSummaryDelta, responsesEventOutputTextDelta:
					assert.Truef(t, open[itemID], "%s for %q arrived without an active item", event.Type, itemID)
				}
			}
			assert.Empty(t, open)
			assert.Equal(t, 2, partAdded)
			assert.Equal(t, 2, partDone)

			completed := events[len(events)-1]
			require.Equal(t, responsesEventCompleted, completed.Type)
			require.NotNil(t, completed.Payload.Response)
			output := completed.Payload.Response.Output
			require.Len(t, output, 5)
			assert.Equal(t, []string{
				responsesOutputTypeReasoning, responsesOutputTypeMessage, responsesOutputTypeFunctionCall,
				responsesOutputTypeReasoning, responsesOutputTypeMessage,
			}, []string{output[0].Type, output[1].Type, output[2].Type, output[3].Type, output[4].Type})
			assert.Equal(t, responsesItemID("rs", "resp_1", 0), output[0].ID)
			assert.Equal(t, "round 1", output[0].Summary[0].Text)
			assert.Equal(t, responsesMessageID("resp_1", 0), output[1].ID)
			assert.Equal(t, "calling", output[1].Content[0].Text)
			assert.Equal(t, "call_1", output[2].CallId)
			assert.Equal(t, responsesItemID("rs", "resp_1", 1), output[3].ID)
			assert.Equal(t, "round 2", output[3].Summary[0].Text)
			assert.Equal(t, responsesMessageID("resp_1", 1), output[4].ID)
			assert.Equal(t, "final", output[4].Content[0].Text)
			for _, item := range output {
				assert.Equal(t, "completed", item.Status)
			}
		})
	}
}

func execCustomToolState() *convmeta.ResponsesToolState {
	return &convmeta.ResponsesToolState{CustomToolNames: map[string]struct{}{"exec": {}}}
}

func TestChatCompletionsResponseToResponsesRestoresCustomToolCalls(t *testing.T) {
	message := dto.Message{Role: "assistant"}
	message.SetToolCalls([]dto.ToolCallRequest{
		{ID: "call_exec", Type: "function", Function: dto.FunctionRequest{Name: "exec", Arguments: `{"input":"echo \"hi\" && ls"}`}},
		{ID: "call_wait", Type: "function", Function: dto.FunctionRequest{Name: "wait", Arguments: `{"ms":1}`}},
	})
	chat := &dto.OpenAITextResponse{
		Model:   "gpt-test",
		Choices: []dto.OpenAITextResponseChoice{{Message: message, FinishReason: "tool_calls"}},
	}

	resp, _, err := ChatCompletionsResponseToResponsesResponseWithTools(chat, "resp_1", execCustomToolState())
	require.NoError(t, err)
	require.Len(t, resp.Output, 2)
	custom, err := kitutil.Marshal(resp.Output[0])
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"type":"custom_tool_call","id":%q,"status":"completed","call_id":"call_exec","name":"exec","input":"echo \"hi\" && ls"}`, responsesItemID("ctc", "resp_1", 0)), string(custom))
	assert.Equal(t, responsesOutputTypeFunctionCall, resp.Output[1].Type)
	assert.Equal(t, `"{\"ms\":1}"`, string(resp.Output[1].Arguments))

	plain, _, err := ChatCompletionsResponseToResponsesResponse(chat, "resp_1")
	require.NoError(t, err)
	assert.Equal(t, responsesOutputTypeFunctionCall, plain.Output[0].Type)
}

func TestCustomToolInputFromArguments(t *testing.T) {
	tests := map[string]string{
		`{"input":"ls -la"}`:          "ls -la",
		`{"input":""}`:                "",
		`{"input":{"cmd":"ls"}}`:      `{"cmd":"ls"}`,
		`{"cmd":"ls"}`:                `{"cmd":"ls"}`,
		`ls -la`:                      "ls -la",
		``:                            "",
		`{"input":"a","extra":true}`:  "a",
		`{"input":"\u003cdiv\u003e"}`: "<div>",
	}
	for arguments, want := range tests {
		assert.Equalf(t, want, customToolInputFromArguments(arguments), "arguments %q", arguments)
	}
}

func TestChatCompletionsStreamToResponsesRestoresCustomToolCalls(t *testing.T) {
	state := NewChatToResponsesStreamState("resp_1", "gpt-test")
	state.EmitSequenceNumber = true
	state.Tools = execCustomToolState()
	execIndex, waitIndex := 0, 1

	var events []ChatToResponsesStreamEvent
	for _, delta := range []dto.ToolCallResponse{
		{Index: &execIndex, ID: "call_exec", Type: "function", Function: dto.FunctionResponse{Name: "exec"}},
		{Index: &execIndex, Function: dto.FunctionResponse{Arguments: `{"input":"echo `}},
		{Index: &execIndex, Function: dto.FunctionResponse{Arguments: `\"hi\""}`}},
		{Index: &waitIndex, ID: "call_wait", Type: "function", Function: dto.FunctionResponse{Name: "wait", Arguments: `{"ms":1}`}},
	} {
		events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
			Choices: []dto.ChatCompletionsStreamResponseChoice{
				{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{delta}}},
			},
		})...)
	}
	events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	assert.Equal(t, []string{
		responsesEventCreated,
		responsesEventOutputItemAdded,
		responsesEventOutputItemAdded,
		responsesEventFunctionArgsDelta,
		responsesEventCustomToolInputDelta,
		responsesEventCustomToolInputDone,
		responsesEventOutputItemDone,
		responsesEventFunctionArgsDone,
		responsesEventOutputItemDone,
		responsesEventCompleted,
	}, types)

	added, err := kitutil.Marshal(events[1].Payload.Item)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"type":"custom_tool_call","id":%q,"status":"in_progress","call_id":"call_exec","name":"exec","input":""}`, responsesItemID("ctc", "resp_1", 0)), string(added))
	assert.Equal(t, `echo "hi"`, events[4].Payload.Delta)
	assert.Equal(t, responsesItemID("ctc", "resp_1", 0), events[4].Payload.ItemID)
	require.NotNil(t, events[5].Payload.Input)
	assert.Equal(t, `echo "hi"`, *events[5].Payload.Input)
	assert.Equal(t, responsesOutputTypeCustomToolCall, events[6].Payload.Item.Type)
	assert.Equal(t, `"echo \"hi\""`, string(events[6].Payload.Item.Input))

	output := events[len(events)-1].Payload.Response.Output
	require.Len(t, output, 2)
	assert.Equal(t, responsesOutputTypeCustomToolCall, output[0].Type)
	assert.Equal(t, `"echo \"hi\""`, string(output[0].Input))
	assert.Equal(t, responsesOutputTypeFunctionCall, output[1].Type)
	assert.Equal(t, `"{\"ms\":1}"`, string(output[1].Arguments))
}

func TestChatCompletionsStreamToResponsesHoldsNamelessToolUntilNameArrives(t *testing.T) {
	toolIndex := 0
	chunk := func(call dto.ToolCallResponse) *dto.ChatCompletionsStreamResponse {
		return &dto.ChatCompletionsStreamResponse{
			Choices: []dto.ChatCompletionsStreamResponseChoice{
				{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{call}}},
			},
		}
	}

	t.Run("custom tool", func(t *testing.T) {
		state := NewChatToResponsesStreamState("resp_1", "gpt-test")
		state.Tools = execCustomToolState()
		first := mustResponsesEventsFromChatChunk(t, state, chunk(dto.ToolCallResponse{Index: &toolIndex, ID: "call_exec", Function: dto.FunctionResponse{Arguments: `{"input":`}}))
		for _, event := range first {
			assert.NotEqual(t, responsesEventOutputItemAdded, event.Type)
			assert.NotEqual(t, responsesEventFunctionArgsDelta, event.Type)
		}
		second := mustResponsesEventsFromChatChunk(t, state, chunk(dto.ToolCallResponse{Index: &toolIndex, Function: dto.FunctionResponse{Name: "exec", Arguments: `"ls"}`}}))
		require.Len(t, second, 1)
		assert.Equal(t, responsesEventOutputItemAdded, second[0].Type)
		assert.Equal(t, responsesOutputTypeCustomToolCall, second[0].Payload.Item.Type)
		done := FinalizeChatCompletionsStreamToResponses(state)
		require.NotEmpty(t, done)
		output := done[len(done)-1].Payload.Response.Output
		require.Len(t, output, 1)
		assert.Equal(t, `"ls"`, string(output[0].Input))
	})

	t.Run("function tool flushes held arguments", func(t *testing.T) {
		state := NewChatToResponsesStreamState("resp_1", "gpt-test")
		first := mustResponsesEventsFromChatChunk(t, state, chunk(dto.ToolCallResponse{Index: &toolIndex, Type: "function", Function: dto.FunctionResponse{Arguments: `{"ms":`}}))
		require.Len(t, first, 1)
		assert.Equal(t, responsesEventCreated, first[0].Type)
		second := mustResponsesEventsFromChatChunk(t, state, chunk(dto.ToolCallResponse{Index: &toolIndex, ID: "call_wait", Function: dto.FunctionResponse{Name: "wait", Arguments: `1}`}}))
		require.Len(t, second, 2)
		assert.Equal(t, responsesEventOutputItemAdded, second[0].Type)
		assert.Equal(t, responsesOutputTypeFunctionCall, second[0].Payload.Item.Type)
		assert.Equal(t, responsesItemID("fc", "resp_1", 0), second[0].Payload.Item.ID)
		assert.Equal(t, "call_wait", second[0].Payload.Item.CallId)
		assert.Equal(t, responsesEventFunctionArgsDelta, second[1].Type)
		assert.Equal(t, responsesItemID("fc", "resp_1", 0), second[1].Payload.ItemID)
		assert.Equal(t, `{"ms":1}`, second[1].Payload.Delta)
		done := FinalizeChatCompletionsStreamToResponses(state)
		output := done[len(done)-1].Payload.Response.Output
		require.Len(t, output, 1)
		assert.Equal(t, responsesItemID("fc", "resp_1", 0), output[0].ID)
		assert.Equal(t, `"{\"ms\":1}"`, string(output[0].Arguments))
	})

	t.Run("held tool takes the next output index when announced", func(t *testing.T) {
		state := NewChatToResponsesStreamState("resp_1", "gpt-test")
		state.Tools = execCustomToolState()
		waitIndex := 1
		var events []ChatToResponsesStreamEvent
		events = append(events, mustResponsesEventsFromChatChunk(t, state, chunk(dto.ToolCallResponse{Index: &toolIndex, ID: "call_exec"}))...)
		events = append(events, mustResponsesEventsFromChatChunk(t, state, chunk(dto.ToolCallResponse{Index: &waitIndex, ID: "call_wait", Function: dto.FunctionResponse{Name: "wait", Arguments: `{}`}}))...)
		events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
			Choices: []dto.ChatCompletionsStreamResponseChoice{{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{Content: lo.ToPtr("done")}}},
		})...)
		events = append(events, mustResponsesEventsFromChatChunk(t, state, chunk(dto.ToolCallResponse{Index: &toolIndex, Function: dto.FunctionResponse{Name: "exec", Arguments: `{"input":"ls"}`}}))...)
		events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

		var addedIndexes []int
		var addedTypes []string
		for _, event := range events {
			if event.Type == responsesEventOutputItemAdded {
				addedIndexes = append(addedIndexes, lo.FromPtr(event.Payload.OutputIndex))
				addedTypes = append(addedTypes, event.Payload.Item.Type)
			}
		}
		assert.Equal(t, []int{0, 1, 2}, addedIndexes)
		assert.Equal(t, []string{responsesOutputTypeFunctionCall, responsesOutputTypeMessage, responsesOutputTypeCustomToolCall}, addedTypes)
		output := events[len(events)-1].Payload.Response.Output
		require.Len(t, output, 3)
		assert.Equal(t, addedTypes, []string{output[0].Type, output[1].Type, output[2].Type})
	})

	t.Run("name never arrives", func(t *testing.T) {
		for _, tools := range []*convmeta.ResponsesToolState{nil, execCustomToolState()} {
			state := NewChatToResponsesStreamState("resp_1", "gpt-test")
			state.EmitSequenceNumber = true
			state.Tools = tools
			missingIndex, validIndex, blankIndex := 0, 1, 2
			var events []ChatToResponsesStreamEvent
			events = append(events, mustResponsesEventsFromChatChunk(t, state, &dto.ChatCompletionsStreamResponse{
				Choices: []dto.ChatCompletionsStreamResponseChoice{{Index: 0, Delta: dto.ChatCompletionsStreamResponseChoiceDelta{ToolCalls: []dto.ToolCallResponse{
					{Index: &missingIndex, ID: "call_missing", Type: "function", Function: dto.FunctionResponse{Arguments: `{"ok":true}`}},
					{Index: &validIndex, ID: "call_valid", Type: "function", Function: dto.FunctionResponse{Name: "lookup", Arguments: `{"q":"x"}`}},
					{Index: &blankIndex, ID: "call_blank", Function: dto.FunctionResponse{Name: " \t\n", Arguments: `{}`}},
				}}}},
			})...)
			events = append(events, FinalizeChatCompletionsStreamToResponses(state)...)

			for i, event := range events {
				require.NotNil(t, event.Payload.SequenceNumber)
				assert.Equal(t, i, *event.Payload.SequenceNumber)
				if event.Payload.OutputIndex != nil {
					assert.Equal(t, 0, *event.Payload.OutputIndex)
				}
				if event.Payload.Item != nil {
					assert.Equal(t, "call_valid", event.Payload.Item.CallId)
				}
			}
			output := events[len(events)-1].Payload.Response.Output
			require.Len(t, output, 1)
			assert.Equal(t, "call_valid", output[0].CallId)
			assert.Equal(t, "lookup", output[0].Name)
		}
	})
}

func mustResponsesEventsFromChatChunk(t *testing.T, state *ChatToResponsesStreamState, chunk *dto.ChatCompletionsStreamResponse) []ChatToResponsesStreamEvent {
	t.Helper()
	events, err := ChatCompletionsStreamChunkToResponsesEvents(chunk, state)
	require.NoError(t, err)
	return events
}
