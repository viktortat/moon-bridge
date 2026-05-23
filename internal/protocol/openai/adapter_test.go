package openai_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"moonbridge/internal/extension/codextool"
	"moonbridge/internal/format"
	"moonbridge/internal/protocol/openai"
)

func TestToCoreRequest_BasicText(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})

	req := &openai.ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`"hello"`),
	}

	result, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if result.Model != "gpt-4o" {
		t.Errorf("Model = %q", result.Model)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("got %d messages", len(result.Messages))
	}
	if result.Messages[0].Role != "user" {
		t.Errorf("Role = %q", result.Messages[0].Role)
	}
	if len(result.Messages[0].Content) != 1 {
		t.Fatalf("got %d content blocks", len(result.Messages[0].Content))
	}
	if result.Messages[0].Content[0].Text != "hello" {
		t.Errorf("Text = %q", result.Messages[0].Content[0].Text)
	}
}

func TestToCoreRequest_WithInstructions(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})

	req := &openai.ResponsesRequest{
		Model:        "gpt-4o",
		Input:        json.RawMessage(`"hello"`),
		Instructions: "Be concise.",
	}

	result, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.System) == 0 {
		t.Fatal("expected system blocks")
	}
	if result.System[0].Text != "Be concise." {
		t.Errorf("System text = %q", result.System[0].Text)
	}
}

func TestToCoreRequest_AppendsInjectedTools(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{
		InjectTools: func(context.Context) []format.CoreTool {
			return []format.CoreTool{{
				Name:        "visual_brief",
				Description: "inspect attached image",
				InputSchema: map[string]any{"type": "object"},
			}}
		},
	})

	req := &openai.ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`"describe the attached image"`),
	}

	result, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 1 {
		t.Fatalf("got %d tools, want 1: %+v", len(result.Tools), result.Tools)
	}
	if result.Tools[0].Name != "visual_brief" {
		t.Fatalf("tool name = %q, want visual_brief", result.Tools[0].Name)
	}
}

func TestToCoreRequest_FunctionCallOutputImage(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})

	req := &openai.ResponsesRequest{
		Model: "gpt-4o",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_view","name":"view_image","arguments":"{\"path\":\"dog.jpg\"}"},
			{"type":"function_call_output","call_id":"call_view","output":[
				{"type":"input_image","image_url":"data:image/jpeg;base64,abc123","detail":"original"}
			]}
		]`),
	}

	result, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("messages = %d, want 2: %+v", len(result.Messages), result.Messages)
	}
	toolResult := result.Messages[1].Content[0]
	if toolResult.Type != "tool_result" || toolResult.ToolUseID != "call_view" {
		t.Fatalf("tool result = %+v", toolResult)
	}
	if len(toolResult.ToolResultContent) != 1 {
		t.Fatalf("tool result content = %+v", toolResult.ToolResultContent)
	}
	image := toolResult.ToolResultContent[0]
	if image.Type != "image" || image.ImageData != "data:image/jpeg;base64,abc123" || image.MediaType != "image/jpeg" {
		t.Fatalf("image block = %+v", image)
	}
}

func TestFromCoreResponse_Basic(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})

	coreResp := &format.CoreResponse{
		ID:     "resp_123",
		Status: "completed",
		Model:  "gpt-4o",
		Messages: []format.CoreMessage{
			{Role: "assistant", Content: []format.CoreContentBlock{{Type: "text", Text: "Hello!"}}},
		},
		Usage: format.CoreUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
	}

	result, err := adapter.FromCoreResponse(context.Background(), coreResp)
	if err != nil {
		t.Fatal(err)
	}

	resp, ok := result.(*openai.Response)
	if !ok {
		t.Fatal("expected *openai.Response")
	}

	if resp.ID != "resp_123" {
		t.Errorf("ID = %q", resp.ID)
	}
	if resp.Status != "completed" {
		t.Errorf("Status = %q", resp.Status)
	}
	if len(resp.Output) != 1 {
		t.Fatalf("Output len=%d, want 1", len(resp.Output))
	}
	if !strings.HasPrefix(resp.Output[0].ID, "msg_123_") {
		t.Fatalf("message output id = %q, want msg_123_*", resp.Output[0].ID)
	}
}

func TestFromCoreResponse_SynthesizesStableResponseStyleIDs(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})

	coreResp := &format.CoreResponse{
		Status: "completed",
		Model:  "deepseek-v4-pro",
		Messages: []format.CoreMessage{
			{Role: "assistant", Content: []format.CoreContentBlock{
				{Type: "reasoning", ReasoningText: "think"},
				{Type: "text", Text: "before"},
				{Type: "tool_use", ToolUseID: "call_1", ToolName: "exec_command", ToolInput: json.RawMessage(`{"cmd":"pwd"}`)},
				{Type: "text", Text: "after"},
			}},
		},
	}

	result, err := adapter.FromCoreResponse(context.Background(), coreResp)
	if err != nil {
		t.Fatal(err)
	}
	resp := result.(*openai.Response)

	if !strings.HasPrefix(resp.ID, "resp_") {
		t.Fatalf("response id = %q, want resp_*", resp.ID)
	}
	if len(resp.Output) != 4 {
		t.Fatalf("Output len=%d, want 4: %+v", len(resp.Output), resp.Output)
	}
	wantPrefixes := []string{"rs_", "msg_", "fc_", "msg_"}
	seen := make(map[string]bool)
	for i, item := range resp.Output {
		if !strings.HasPrefix(item.ID, wantPrefixes[i]) {
			t.Fatalf("output[%d].ID = %q, want prefix %q", i, item.ID, wantPrefixes[i])
		}
		if seen[item.ID] {
			t.Fatalf("duplicate output id %q in %+v", item.ID, resp.Output)
		}
		seen[item.ID] = true
	}
	if resp.Output[2].CallID != "call_1" {
		t.Fatalf("tool call_id = %q, want call_1", resp.Output[2].CallID)
	}
	if resp.Output[2].ID == resp.Output[2].CallID {
		t.Fatalf("tool item id should differ from call_id: %+v", resp.Output[2])
	}
}

func TestFromCoreResponse_Error(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})

	coreResp := &format.CoreResponse{
		Status: "failed",
		Error:  &format.CoreError{Message: "upstream error", Code: "api_error"},
	}

	result, err := adapter.FromCoreResponse(context.Background(), coreResp)
	if err != nil {
		t.Fatal(err)
	}
	resp := result.(*openai.Response)

	if resp.Status != "failed" {
		t.Errorf("Status = %q", resp.Status)
	}
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	if resp.Error.Message != "upstream error" {
		t.Errorf("Error.Message = %q", resp.Error.Message)
	}
}

func TestToCoreRequest_NilInput(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})

	req := &openai.ResponsesRequest{
		Model: "gpt-4o",
		Input: nil,
	}

	_, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
}

func TestToCoreRequest_ReasoningModelInjectsEmptyReasoningBeforeFunctionCall(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})
	req := &openai.ResponsesRequest{
		Model: "o3-mini",
		Input: json.RawMessage(`[
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"}
		]`),
	}
	result, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages len=%d, want 1", len(result.Messages))
	}
	if len(result.Messages[0].Content) < 2 {
		t.Fatalf("assistant content len=%d, want >=2", len(result.Messages[0].Content))
	}
	if result.Messages[0].Content[0].Type != "reasoning" {
		t.Fatalf("first content type=%q, want reasoning", result.Messages[0].Content[0].Type)
	}
	if result.Messages[0].Content[1].Type != "tool_use" {
		t.Fatalf("second content type=%q, want tool_use", result.Messages[0].Content[1].Type)
	}
}

func TestToCoreRequest_KeepsToolUseAdjacentToToolResultWhenReasoningPrecedesOutput(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})
	req := &openai.ResponsesRequest{
		Model: "gpt-5.4",
		Input: json.RawMessage(`[
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool_a","arguments":"{\"a\":1}"},
			{"type":"reasoning","summary":[{"type":"text","text":"thinking after tool call"}]},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]`),
	}

	result, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("messages len=%d, want 2; got %+v", len(result.Messages), result.Messages)
	}

	assistant := result.Messages[0]
	if assistant.Role != "assistant" {
		t.Fatalf("messages[0].Role=%q, want assistant", assistant.Role)
	}
	if len(assistant.Content) != 2 {
		t.Fatalf("assistant content len=%d, want 2; got %+v", len(assistant.Content), assistant.Content)
	}
	if assistant.Content[0].Type != "reasoning" || assistant.Content[0].ReasoningText != "thinking after tool call" {
		t.Fatalf("assistant.Content[0]=%+v, want merged reasoning", assistant.Content[0])
	}
	if assistant.Content[1].Type != "tool_use" || assistant.Content[1].ToolUseID != "call_1" {
		t.Fatalf("assistant.Content[1]=%+v, want tool_use call_1", assistant.Content[1])
	}

	toolResult := result.Messages[1]
	if toolResult.Role != "tool" {
		t.Fatalf("messages[1].Role=%q, want tool", toolResult.Role)
	}
	if len(toolResult.Content) != 1 || toolResult.Content[0].Type != "tool_result" || toolResult.Content[0].ToolUseID != "call_1" {
		t.Fatalf("tool result message=%+v", toolResult)
	}
}

func TestToCoreRequest_BatchesCustomToolCallsAndOutputsIntoSingleRound(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})
	req := &openai.ResponsesRequest{
		Model: "gpt-5.4",
		Input: json.RawMessage(`[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before tools"}]},
			{"type":"custom_tool_call","call_id":"call_a","name":"apply_patch","input":"patch a","arguments":"{\"input\":\"patch a\"}"},
			{"type":"custom_tool_call_output","call_id":"call_a","output":"ok a"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"between tools"}]},
			{"type":"custom_tool_call","call_id":"call_b","name":"apply_patch","input":"patch b","arguments":"{\"input\":\"patch b\"}"},
			{"type":"custom_tool_call_output","call_id":"call_b","output":"ok b"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"between tools 2"}]},
			{"type":"custom_tool_call","call_id":"call_c","name":"apply_patch","input":"patch c","arguments":"{\"input\":\"patch c\"}"},
			{"type":"custom_tool_call_output","call_id":"call_c","output":"ok c"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after tools"}]}
		]`),
	}

	result, err := adapter.ToCoreRequest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Messages) != 10 {
		t.Fatalf("messages len=%d, want 10; got %+v", len(result.Messages), result.Messages)
	}

	if result.Messages[0].Role != "assistant" || len(result.Messages[0].Content) != 1 || result.Messages[0].Content[0].Text != "before tools" {
		t.Fatalf("messages[0]=%+v, want pre-tool assistant text", result.Messages[0])
	}

	for i, want := range []struct {
		assistantTextIdx int
		msgIdx           int
		callID           string
		outcome          string
	}{
		{0, 1, "call_a", "ok a"},
		{3, 4, "call_b", "ok b"},
		{6, 7, "call_c", "ok c"},
	} {
		if result.Messages[want.assistantTextIdx].Role != "assistant" {
			t.Fatalf("assistant commentary turn %d = %+v", i, result.Messages[want.assistantTextIdx])
		}
		assistant := result.Messages[want.msgIdx]
		if assistant.Role != "assistant" || len(assistant.Content) != 1 || assistant.Content[0].Type != "tool_use" || assistant.Content[0].ToolUseID != want.callID {
			t.Fatalf("assistant tool turn %d = %+v", i, assistant)
		}
		toolResult := result.Messages[want.msgIdx+1]
		if toolResult.Role != "tool" || len(toolResult.Content) != 1 || toolResult.Content[0].Type != "tool_result" || toolResult.Content[0].ToolUseID != want.callID {
			t.Fatalf("tool result turn %d = %+v", i, toolResult)
		}
		if got := toolResult.Content[0].ToolResultContent[0].Text; got != want.outcome {
			t.Fatalf("tool result text turn %d = %q, want %q", i, got, want.outcome)
		}
	}

	if result.Messages[9].Role != "assistant" || len(result.Messages[9].Content) != 1 || result.Messages[9].Content[0].Text != "after tools" {
		t.Fatalf("messages[9]=%+v, want trailing assistant text", result.Messages[9])
	}
}

func TestFromCoreStream_NoDuplicateDoneForToolUse(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})
	coreReq := &format.CoreRequest{Model: "gpt-4o"}
	evCh := make(chan format.CoreStreamEvent, 8)
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockStarted,
		Index: 5,
		ContentBlock: &format.CoreContentBlock{
			Type:      "tool_use",
			ToolUseID: "call_1",
			ToolName:  "exec_command",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreToolCallArgsDelta, Index: 5, Delta: `{"cmd":"ls"}`}
	evCh <- format.CoreStreamEvent{Type: format.CoreToolCallArgsDone, Index: 5, Delta: `{"cmd":"ls"}`}
	evCh <- format.CoreStreamEvent{Type: format.CoreContentBlockDone, Index: 5}
	evCh <- format.CoreStreamEvent{Type: format.CoreEventCompleted, Status: "completed"}
	close(evCh)

	streamAny, err := adapter.FromCoreStream(context.Background(), coreReq, evCh)
	if err != nil {
		t.Fatal(err)
	}
	stream := streamAny.(<-chan openai.StreamEvent)
	var argsDone int
	var itemDone int
	var toolItemID string
	for ev := range stream {
		switch ev.Event {
		case "response.output_item.added":
			data := ev.Data.(openai.OutputItemEvent)
			if data.Item.Type == "function_call" {
				toolItemID = data.Item.ID
				if !strings.HasPrefix(toolItemID, "fc_") {
					t.Fatalf("tool item id = %q, want fc_*", toolItemID)
				}
				if data.Item.CallID != "call_1" {
					t.Fatalf("tool call_id = %q, want call_1", data.Item.CallID)
				}
				if data.Item.ID == data.Item.CallID {
					t.Fatalf("tool item id should differ from call_id: %+v", data.Item)
				}
			}
		case "response.function_call_arguments.delta":
			data := ev.Data.(openai.FunctionCallArgumentsDeltaEvent)
			if data.ItemID != toolItemID {
				t.Fatalf("args delta item_id = %q, want %q", data.ItemID, toolItemID)
			}
		case "response.function_call_arguments.done":
			data := ev.Data.(openai.FunctionCallArgumentsDoneEvent)
			if data.ItemID != toolItemID {
				t.Fatalf("args done item_id = %q, want %q", data.ItemID, toolItemID)
			}
		}
		if ev.Event == "response.function_call_arguments.done" {
			argsDone++
		}
		if ev.Event == "response.output_item.done" {
			if data, ok := ev.Data.(openai.OutputItemEvent); ok && strings.HasPrefix(data.Item.CallID, "call_") {
				itemDone++
			}
		}
	}
	if argsDone != 1 {
		t.Fatalf("function_call_arguments.done count=%d, want 1", argsDone)
	}
	if itemDone != 1 {
		t.Fatalf("output_item.done (tool) count=%d, want 1", itemDone)
	}
}

func TestFromCoreStream_ReasoningSummaryLifecycle(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})
	coreReq := &format.CoreRequest{Model: "deepseek-v4"}
	evCh := make(chan format.CoreStreamEvent, 8)
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockStarted,
		Index: 1,
		ContentBlock: &format.CoreContentBlock{
			Type: "reasoning",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreTextDelta, Index: 1, Delta: "thinking"}
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockDone,
		Index: 1,
		ContentBlock: &format.CoreContentBlock{
			Type:               "reasoning",
			ReasoningSignature: "sig-1",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreEventCompleted, Status: "completed"}
	close(evCh)

	streamAny, err := adapter.FromCoreStream(context.Background(), coreReq, evCh)
	if err != nil {
		t.Fatal(err)
	}

	var events []string
	var addedPart openai.ReasoningSummaryPartAddedEvent
	var textDone openai.ReasoningSummaryTextDoneEvent
	var partDone openai.ReasoningSummaryPartDoneEvent
	var itemDone openai.OutputItemEvent
	for ev := range streamAny.(<-chan openai.StreamEvent) {
		events = append(events, ev.Event)
		switch ev.Event {
		case "response.reasoning_summary_part.added":
			addedPart = ev.Data.(openai.ReasoningSummaryPartAddedEvent)
		case "response.reasoning_summary_text.done":
			textDone = ev.Data.(openai.ReasoningSummaryTextDoneEvent)
		case "response.reasoning_summary_part.done":
			partDone = ev.Data.(openai.ReasoningSummaryPartDoneEvent)
		case "response.output_item.done":
			if data := ev.Data.(openai.OutputItemEvent); data.Item.Type == "reasoning" {
				itemDone = data
			}
		}
	}

	want := []string{
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
	if addedPart.Part.Type != "summary_text" {
		t.Fatalf("added part = %+v, want summary_text", addedPart.Part)
	}
	if textDone.Text != "thinking" {
		t.Fatalf("text done = %q, want thinking", textDone.Text)
	}
	if partDone.Part.Type != "summary_text" || partDone.Part.Text != "thinking" || partDone.Part.Signature != "sig-1" {
		t.Fatalf("part done = %+v, want completed reasoning part", partDone.Part)
	}
	if itemDone.Item.Status != "completed" || len(itemDone.Item.Summary) != 1 || itemDone.Item.Summary[0].Text != "thinking" {
		t.Fatalf("item done = %+v, want completed reasoning item", itemDone.Item)
	}
}

func TestFromCoreStream_CustomToolUsesCustomInputEvents(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})
	coreReq := &format.CoreRequest{
		Model: "deepseek-v4",
		Extensions: map[string]any{
			"codex_tool_map": codextool.ToolMap{
				"apply_patch": {
					Kind:       codextool.ToolRaw,
					OpenAIName: "apply_patch",
				},
			}.Encode(),
		},
	}
	evCh := make(chan format.CoreStreamEvent, 8)
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockStarted,
		Index: 2,
		ContentBlock: &format.CoreContentBlock{
			Type:      "tool_use",
			ToolUseID: "call_patch",
			ToolName:  "apply_patch",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreToolCallArgsDelta, Index: 2, Delta: `{"input":"*** Begin Patch\n*** End Patch"}`}
	evCh <- format.CoreStreamEvent{Type: format.CoreContentBlockDone, Index: 2}
	evCh <- format.CoreStreamEvent{Type: format.CoreEventCompleted, Status: "completed"}
	close(evCh)

	streamAny, err := adapter.FromCoreStream(context.Background(), coreReq, evCh)
	if err != nil {
		t.Fatal(err)
	}

	var customDelta int
	var customDone int
	var functionArgsDone int
	for ev := range streamAny.(<-chan openai.StreamEvent) {
		switch ev.Event {
		case "response.custom_tool_call_input.delta":
			customDelta++
			data := ev.Data.(openai.CustomToolCallInputDeltaEvent)
			if data.CallID != "call_patch" || data.Delta == "" {
				t.Fatalf("custom delta = %+v", data)
			}
		case "response.custom_tool_call_input.done":
			customDone++
			data := ev.Data.(openai.CustomToolCallInputDoneEvent)
			if data.CallID != "call_patch" || data.Input != "*** Begin Patch\n*** End Patch" {
				t.Fatalf("custom done = %+v", data)
			}
		case "response.function_call_arguments.done":
			functionArgsDone++
		}
	}
	if customDelta != 1 {
		t.Fatalf("custom_tool_call_input.delta count=%d, want 1", customDelta)
	}
	if customDone != 1 {
		t.Fatalf("custom_tool_call_input.done count=%d, want 1", customDone)
	}
	if functionArgsDone != 0 {
		t.Fatalf("function_call_arguments.done count=%d, want 0", functionArgsDone)
	}
}

func TestFromCoreStream_InterleavedTextAndToolsLifecycle(t *testing.T) {
	adapter := openai.NewOpenAIAdapter(format.CorePluginHooks{})
	coreReq := &format.CoreRequest{
		Model: "deepseek-v4-pro",
		Extensions: map[string]any{
			"codex_tool_map": codextool.ToolMap{
				"apply_patch": {
					Kind:       codextool.ToolRaw,
					OpenAIName: "apply_patch",
				},
			}.Encode(),
		},
	}

	// Simulate DeepSeek stream: thinking → text → tool_use → text
	evCh := make(chan format.CoreStreamEvent, 20)
	evCh <- format.CoreStreamEvent{Type: format.CoreEventCreated, Status: "in_progress", Model: "deepseek-v4-pro"}

	// 1. thinking / reasoning_content block
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockStarted,
		Index: 0,
		ContentBlock: &format.CoreContentBlock{
			Type:          "reasoning",
			ReasoningText: "I need to check the code first.",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreTextDelta, Index: 0, Delta: "I need to check the code first."}
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockDone,
		Index: 0,
		ContentBlock: &format.CoreContentBlock{
			Type:               "reasoning",
			ReasoningSignature: "deepseek-sig-abc",
		},
	}

	// 2. first text block
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockStarted,
		Index: 1,
		ContentBlock: &format.CoreContentBlock{
			Type: "text",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreTextDelta, Index: 1, Delta: "Let me read the file."}
	evCh <- format.CoreStreamEvent{Type: format.CoreContentBlockDone, Index: 1}

	// 3. tool_use block
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockStarted,
		Index: 2,
		ContentBlock: &format.CoreContentBlock{
			Type:      "tool_use",
			ToolUseID: "call_patch",
			ToolName:  "apply_patch",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreToolCallArgsDelta, Index: 2, Delta: `{"input":"*** Begin Patch\n*** End Patch"}`}
	evCh <- format.CoreStreamEvent{Type: format.CoreContentBlockDone, Index: 2}

	// 4. second text block
	evCh <- format.CoreStreamEvent{
		Type:  format.CoreContentBlockStarted,
		Index: 3,
		ContentBlock: &format.CoreContentBlock{
			Type: "text",
		},
	}
	evCh <- format.CoreStreamEvent{Type: format.CoreTextDelta, Index: 3, Delta: "The change looks correct."}
	evCh <- format.CoreStreamEvent{Type: format.CoreContentBlockDone, Index: 3}

	evCh <- format.CoreStreamEvent{Type: format.CoreEventCompleted, Status: "completed"}
	close(evCh)

	streamAny, err := adapter.FromCoreStream(context.Background(), coreReq, evCh)
	if err != nil {
		t.Fatal(err)
	}

	var events []string
	var completed *openai.ResponseLifecycleEvent
	var responseCreated *openai.ResponseLifecycleEvent
	var responseInProgress *openai.ResponseLifecycleEvent
	var customToolID string
	for ev := range streamAny.(<-chan openai.StreamEvent) {
		events = append(events, ev.Event)
		switch ev.Event {
		case "response.created":
			data := ev.Data.(openai.ResponseLifecycleEvent)
			responseCreated = &data
		case "response.in_progress":
			data := ev.Data.(openai.ResponseLifecycleEvent)
			responseInProgress = &data
		case "response.custom_tool_call_input.delta":
			data := ev.Data.(openai.CustomToolCallInputDeltaEvent)
			if data.CallID != "call_patch" {
				t.Fatalf("custom delta call_id = %q, want call_patch", data.CallID)
			}
			if !strings.HasPrefix(data.ItemID, "fc_") {
				t.Fatalf("custom delta item_id = %q, want fc_*", data.ItemID)
			}
			customToolID = data.ItemID
		case "response.custom_tool_call_input.done":
			data := ev.Data.(openai.CustomToolCallInputDoneEvent)
			if data.CallID != "call_patch" {
				t.Fatalf("custom done call_id = %q, want call_patch", data.CallID)
			}
			if data.ItemID != customToolID {
				t.Fatalf("custom done item_id = %q, want %q", data.ItemID, customToolID)
			}
		case "response.completed":
			data := ev.Data.(openai.ResponseLifecycleEvent)
			completed = &data
		}
	}

	// Verify event order follows the expected interleaved lifecycle.
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",  // reasoning completed
		"response.output_item.added", // text 1
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",  // text 1 completed
		"response.output_item.added", // custom tool
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",  // custom tool completed
		"response.output_item.added", // text 2
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done", // text 2 completed
		"response.completed",
	}
	if !reflect.DeepEqual(events, wantOrder) {
		t.Fatalf("events = %#v\nwant %#v", events, wantOrder)
	}
	if responseCreated == nil || !strings.HasPrefix(responseCreated.Response.ID, "resp_") {
		t.Fatalf("response.created = %+v, want resp_* id", responseCreated)
	}
	if responseInProgress == nil || responseInProgress.Response.ID != responseCreated.Response.ID {
		t.Fatalf("response.in_progress = %+v, want same response id as created %+v", responseInProgress, responseCreated)
	}

	// Verify response.Output in completed event has all four items in correct order.
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	resp := completed.Response
	if len(resp.Output) != 4 {
		t.Fatalf("Output len=%d, want 4: %+v", len(resp.Output), resp.Output)
	}
	if resp.ID != responseCreated.Response.ID {
		t.Fatalf("completed response id = %q, want %q", resp.ID, responseCreated.Response.ID)
	}
	if want := []string{"reasoning", "message", "custom_tool_call", "message"}; !reflect.DeepEqual(
		[]string{resp.Output[0].Type, resp.Output[1].Type, resp.Output[2].Type, resp.Output[3].Type},
		want,
	) {
		t.Fatalf("Output types = %v, want %v",
			[]string{resp.Output[0].Type, resp.Output[1].Type, resp.Output[2].Type, resp.Output[3].Type}, want)
	}
	if resp.Output[0].Status != "completed" || resp.Output[1].Status != "completed" ||
		resp.Output[2].Status != "completed" || resp.Output[3].Status != "completed" {
		t.Fatalf("all Output items should be completed: %+v", resp.Output)
	}
	wantPrefixes := []string{"rs_", "msg_", "fc_", "msg_"}
	seen := make(map[string]bool)
	for i, item := range resp.Output {
		if !strings.HasPrefix(item.ID, wantPrefixes[i]) {
			t.Fatalf("output[%d].ID = %q, want prefix %q", i, item.ID, wantPrefixes[i])
		}
		if seen[item.ID] {
			t.Fatalf("duplicate output item id %q in %+v", item.ID, resp.Output)
		}
		seen[item.ID] = true
	}
	if resp.Output[2].ID != customToolID {
		t.Fatalf("custom tool item id = %q, want event item_id %q", resp.Output[2].ID, customToolID)
	}
	if resp.Output[2].CallID != "call_patch" {
		t.Fatalf("custom tool call_id = %q, want call_patch", resp.Output[2].CallID)
	}
	if resp.Output[2].ID == resp.Output[2].CallID {
		t.Fatalf("custom tool item id should differ from call_id: %+v", resp.Output[2])
	}
	if resp.Output[1].Content == nil || len(resp.Output[1].Content) == 0 || resp.Output[1].Content[0].Text != "Let me read the file." {
		t.Fatalf("text 1 content = %+v", resp.Output[1].Content)
	}
	if resp.Output[3].Content == nil || len(resp.Output[3].Content) == 0 || resp.Output[3].Content[0].Text != "The change looks correct." {
		t.Fatalf("text 2 content = %+v", resp.Output[3].Content)
	}
}
