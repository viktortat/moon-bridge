package server

import (
	"context"
	"testing"

	"moonbridge/internal/format"
)

func TestCoreResponseToCoreStreamEmitsUsageOnCompleted(t *testing.T) {
	resp := &format.CoreResponse{
		ID:     "resp_test",
		Status: "completed",
		Model:  "claude-test",
		Messages: []format.CoreMessage{
			{
				Role: "assistant",
				Content: []format.CoreContentBlock{
					{Type: "text", Text: "hello"},
					{Type: "tool_use", ToolUseID: "call_1", ToolName: "exec_command", ToolInput: []byte(`{"cmd":"ls"}`)},
					{Type: "reasoning", ReasoningText: "think", ReasoningSignature: "sig_1"},
				},
			},
		},
		Usage: format.CoreUsage{
			InputTokens:       11,
			OutputTokens:      7,
			CachedInputTokens: 3,
		},
		StopReason: "end_turn",
	}

	stream := coreResponseToCoreStream(context.Background(), resp)
	var events []format.CoreStreamEvent
	for ev := range stream {
		events = append(events, ev)
	}

	if len(events) == 0 {
		t.Fatal("no stream events emitted")
	}
	if events[0].Type != format.CoreEventCreated {
		t.Fatalf("first event type = %s, want %s", events[0].Type, format.CoreEventCreated)
	}

	var completed *format.CoreStreamEvent
	for i := range events {
		if events[i].Type == format.CoreEventCompleted {
			completed = &events[i]
			break
		}
	}
	if completed == nil {
		t.Fatal("missing core.completed event")
	}
	if completed.Usage == nil {
		t.Fatal("completed usage is nil")
	}
	if completed.Usage.InputTokens != 11 || completed.Usage.OutputTokens != 7 || completed.Usage.CachedInputTokens != 3 {
		t.Fatalf("completed usage = %+v", completed.Usage)
	}
	if completed.Usage.TotalTokens != 18 {
		t.Fatalf("completed usage total_tokens = %d, want 18", completed.Usage.TotalTokens)
	}

	var sawToolStarted bool
	var sawToolArgsDone bool
	for _, ev := range events {
		if ev.Type == format.CoreContentBlockStarted && ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			sawToolStarted = true
		}
		if ev.Type == format.CoreToolCallArgsDone && ev.Delta == `{"cmd":"ls"}` {
			sawToolArgsDone = true
		}
	}
	if !sawToolStarted {
		t.Fatal("missing tool_use block start event")
	}
	if !sawToolArgsDone {
		t.Fatal("missing tool args done event")
	}
}
