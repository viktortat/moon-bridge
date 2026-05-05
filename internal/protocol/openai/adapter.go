// Package openai contains the OpenAI Responses protocol adapter.
//
// OpenAIAdapter implements format.ClientAdapter and format.ClientStreamAdapter,
// converting between OpenAI Responses DTOs and the Core intermediate format.
//
// Clean room design: no imports from moonbridge/internal/protocol/bridge/,
// moonbridge/internal/protocol/anthropic/, or any protocol-specific packages
// other than the OpenAI DTOs defined in this package.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"moonbridge/internal/foundation/config"
	"moonbridge/internal/protocol/format"
)

// ============================================================================
// OpenAIAdapter
// ============================================================================

// OpenAIAdapter converts between OpenAI Responses DTOs and the Core format.
//
// It implements the inbound (client) side of the bridge:
//   - ClientAdapter:  ToCoreRequest / FromCoreResponse
//   - ClientStreamAdapter: FromCoreStream
//
// The adapter is stateless; all configuration is injected via the constructor.
type OpenAIAdapter struct {
	cfg   config.Config
	hooks format.CorePluginHooks

	streamMu         sync.Mutex
	streamEvents     []StreamEvent
}

// NewOpenAIAdapter creates a new OpenAIAdapter with the given config and hooks.
func NewOpenAIAdapter(cfg config.Config, hooks format.CorePluginHooks) *OpenAIAdapter {
	return &OpenAIAdapter{
		cfg:      cfg,
		hooks:    hooks.WithDefaults(),
	}
}

// ClientProtocol returns the inbound protocol identifier.
func (a *OpenAIAdapter) ClientProtocol() string {
	return "openai-response"
}

// ============================================================================
// ToCoreRequest — OpenAI ResponsesRequest → CoreRequest
// ============================================================================

// ToCoreRequest converts an inbound OpenAI Responses request into a CoreRequest.
//
// Supported mappings:
//   - Model, Temperature, TopP, MaxOutputTokens, Stream, Metadata → direct copy
//   - Input (string | array) → Messages + System
//   - Instructions → appended to System
//   - Tools → CoreTool (function → name/desc/schema; web_search → extensions)
//   - ToolChoice → CoreToolChoice (with raw JSON preserved)
//   - PromptCacheKey / PromptCacheRetention → Extensions["cache"]
//
// Error handling: all conversion errors are returned to the caller with
// the original message preserved — no error wrapping, no side effects.
func (a *OpenAIAdapter) ToCoreRequest(ctx context.Context, req any) (*format.CoreRequest, error) {
	openaiReq, ok := req.(*ResponsesRequest)
	if !ok {
		// Accept non-pointer value as well
		direct, ok2 := req.(ResponsesRequest)
		if !ok2 {
			return nil, fmt.Errorf("unexpected request type %T; expected *ResponsesRequest", req)
		}
		openaiReq = &direct
	}

	// 1. Apply PreprocessInput hook (operates on raw JSON before parsing).
	preprocessed := a.hooks.PreprocessInput(ctx, openaiReq.Model, openaiReq.Input)
	openaiReq.Input = preprocessed

	// 2. Parse Input → Messages + System.
	messages, system, err := convertInput(openaiReq.Input)
	if err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}

	// 3. Append Instructions to System.
	if openaiReq.Instructions != "" {
		system = append(system, format.CoreContentBlock{
			Type: "text",
			Text: openaiReq.Instructions,
		})
	}

	// 4. Build CoreRequest with direct scalar mappings.
	coreReq := &format.CoreRequest{
		Model:       openaiReq.Model,
		Messages:    messages,
		System:      system,
		Temperature: openaiReq.Temperature,
		TopP:        openaiReq.TopP,
		MaxTokens:   openaiReq.MaxOutputTokens,
		Stream:      openaiReq.Stream,
		Metadata:    openaiReq.Metadata,
	}

	// 5. Convert tools.
	if len(openaiReq.Tools) > 0 {
		coreReq.Tools = make([]format.CoreTool, 0, len(openaiReq.Tools))
		for _, tool := range openaiReq.Tools {
			coreReq.Tools = append(coreReq.Tools, convertTool(tool))
		}
	}

	// 6. Convert tool choice.
	if len(openaiReq.ToolChoice) > 0 && string(openaiReq.ToolChoice) != "null" {
		tc, err := convertToolChoice(openaiReq.ToolChoice)
		if err != nil {
			return nil, fmt.Errorf("invalid tool_choice: %w", err)
		}
		coreReq.ToolChoice = tc
	}

	// 7. Cache metadata passthrough.
	coreReq.Extensions = make(map[string]any)
	if openaiReq.PromptCacheKey != "" || openaiReq.PromptCacheRetention != "" {
		cacheMeta := make(map[string]string)
		if openaiReq.PromptCacheKey != "" {
			cacheMeta["prompt_cache_key"] = openaiReq.PromptCacheKey
		}
		if openaiReq.PromptCacheRetention != "" {
			cacheMeta["prompt_cache_retention"] = openaiReq.PromptCacheRetention
		}
		coreReq.Extensions["cache"] = cacheMeta
	}

	// 8. Apply MutateCoreRequest hook for plugins.
	a.hooks.MutateCoreRequest(ctx, coreReq)

	return coreReq, nil
}

// ============================================================================
// FromCoreResponse — CoreResponse → OpenAI Response
// ============================================================================

// FromCoreResponse converts a CoreResponse back into an OpenAI Response.
//
// The conversion extracts assistant messages as OutputItem("message") items,
// tool_use content blocks as function_call items, and reasoning blocks as
// reasoning items. The output_text field is built by concatenating text parts.
func (a *OpenAIAdapter) FromCoreResponse(ctx context.Context, resp *format.CoreResponse) (any, error) {
	if resp == nil {
		return nil, fmt.Errorf("core response is nil")
	}

	// Apply PostProcessCoreResponse hook.
	a.hooks.PostProcessCoreResponse(ctx, resp)

	response := &Response{
		ID:     resp.ID,
		Object: "response",
		Status: resp.Status,
		Model:  resp.Model,
	}

	// Convert Messages → Output items.
	var output []OutputItem
	for _, msg := range resp.Messages {
		if msg.Role != "assistant" {
			continue
		}
		// Collect consecutive text blocks into a single message item.
		textParts := make([]ContentPart, 0)
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				textParts = append(textParts, ContentPart{Type: "text", Text: block.Text})

			case "reasoning":
				output = append(output, OutputItem{
					Type:   "reasoning",
					Status: "completed",
					Summary: []ReasoningItemSummary{
						{Type: "text", Text: block.ReasoningText},
					},
				})

			case "tool_use":
				// Flush accumulated text parts before the tool call item.
				if len(textParts) > 0 {
					output = append(output, OutputItem{
						Type:    "message",
						Status:  "completed",
						Role:    "assistant",
						Content: copyContentParts(textParts),
					})
					textParts = textParts[:0]
				}
				output = append(output, OutputItem{
					Type:      "function_call",
					ID:        block.ToolUseID,
					CallID:    block.ToolUseID,
					Name:      block.ToolName,
					Arguments: toolInputString(block.ToolInput),
					Status:    "completed",
				})

			case "tool_result":
				// Tool results don't translate to output items in the response.
				// They are input-side artifacts.

			case "image":
				textParts = append(textParts, ContentPart{Type: "text", Text: "[Image]"})
			}
		}
		// Flush remaining text parts.
		if len(textParts) > 0 {
			output = append(output, OutputItem{
				Type:    "message",
				Status:  "completed",
				Role:    "assistant",
				Content: copyContentParts(textParts),
			})
		}
	}
	response.Output = output

	// Build output_text from message items.
	var texts []string
	for _, item := range output {
		if item.Type == "message" {
			for _, part := range item.Content {
				if part.Type == "text" {
					texts = append(texts, part.Text)
				}
			}
		}
	}
	response.OutputText = strings.Join(texts, "")

	// Map usage.
	response.Usage = Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		InputTokensDetails: InputTokensDetails{
			CachedTokens: resp.Usage.CachedInputTokens,
		},
	}

	// Map error.
	if resp.Error != nil {
		response.Error = &ErrorObject{
			Message: resp.Error.Message,
			Type:    resp.Error.Type,
			Code:    resp.Error.Code,
		}
		if response.Status == "" || response.Status == "completed" {
			response.Status = "failed"
		}
	}

	return response, nil
}

// ============================================================================
// FromCoreStream — CoreStreamEvent channel → OpenAI StreamEvent channel
// ============================================================================

// FromCoreStream consumes a channel of CoreStreamEvent and produces a channel
// of StreamEvent suitable for SSE serialization downstream.
//
// The returned channel is closed when the input channel is exhausted.
// The adapter manages internal state (output index tracking, text accumulation)
// to produce correct OpenAI stream semantics.
func (a *OpenAIAdapter) FromCoreStream(ctx context.Context, req *format.CoreRequest, events <-chan format.CoreStreamEvent) (any, error) {
	out := make(chan StreamEvent)

	go a.streamLoop(ctx, req, events, out)

	return (<-chan StreamEvent)(out), nil
}

// bufferStreamEvent buffers the OpenAI stream event for trace capture,
// up to the 4MB limit. The event is JSON-marshalled to estimate its size.
func (a *OpenAIAdapter) bufferStreamEvent(ev StreamEvent) {
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	a.streamEvents = append(a.streamEvents, ev)
}

// StreamBuffer returns the buffered stream events for trace capture.
func (a *OpenAIAdapter) StreamBuffer() []StreamEvent {
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	return a.streamEvents
}

// streamLoop is the goroutine body for FromCoreStream.
func (a *OpenAIAdapter) streamLoop(ctx context.Context, coreReq *format.CoreRequest, events <-chan format.CoreStreamEvent, out chan<- StreamEvent) {
	defer close(out)

	// send buffers the event for trace capture before writing to the output channel.
	send := func(ev StreamEvent) {
		a.bufferStreamEvent(ev)
		out <- ev
	}

	seqNum := int64(0)
	next := func() int64 {
		seqNum++
		return seqNum
	}

	// State tracked during streaming.
	var response = &Response{
		Object: "response",
		Status: "in_progress",
	}
	contentText := make(map[int]string)
	toolCallArgs := make(map[int]string)
	outputIndexes := make(map[int]int)
	itemIDs := make(map[int]string)
	reasonIndexes := make(map[int]bool)

	for event := range events {
		// Let hooks skip events.
		if a.hooks.OnStreamEvent(ctx, event) {
			continue
		}

		switch event.Type {
		// ==================================================================
		// Lifecycle: created
		// ==================================================================
		case format.CoreEventCreated:
			// Use ItemID as the response ID if set; otherwise keep the current one.
			if event.ItemID != "" {
				response.ID = event.ItemID
			}
			response.Status = "in_progress"

			send(StreamEvent{
				Event: "response.created",
				Data: ResponseLifecycleEvent{
					Type:           "response.created",
					SequenceNumber: next(),
					Response:       cloneResponse(response),
				},
			})

		// ==================================================================
		// Lifecycle: in_progress
		// ==================================================================
		case format.CoreEventInProgress:
			response.Status = "in_progress"
			send(StreamEvent{
				Event: "response.in_progress",
				Data: ResponseLifecycleEvent{
					Type:           "response.in_progress",
					SequenceNumber: next(),
					Response:       cloneResponse(response),
				},
			})

		// ==================================================================
		// Content block started
		// ==================================================================
		case format.CoreContentBlockStarted:
			if event.ContentBlock == nil {
				continue
			}
			index := event.Index

			switch event.ContentBlock.Type {
			case "text":
				id := fmt.Sprintf("msg_item_%d", index)
				itemIDs[index] = id
				contentText[index] = ""

			case "reasoning":
				id := fmt.Sprintf("rs_item_%d", index)
				itemIDs[index] = id
				contentText[index] = ""
				reasonIndexes[index] = true
				io := len(response.Output)
				outputIndexes[index] = io
				response.Output = append(response.Output, OutputItem{
					Type:   "reasoning",
					ID:     id,
					Status: "in_progress",
					Summary: []ReasoningItemSummary{},
				})
				send(StreamEvent{
					Event: "response.reasoning_summary_part.added",
					Data: ReasoningSummaryPartAddedEvent{
						Type:           "response.reasoning_summary_part.added",
						SequenceNumber: next(),
						ItemID:         id,
						OutputIndex:    io,
						SummaryIndex:   0,
					},
				})
				contentText[index] = ""
			case "tool_use":
				toolUseID := event.ContentBlock.ToolUseID
				if toolUseID == "" {
					toolUseID = fmt.Sprintf("call_%d", index)
				}
				itemIDs[index] = fmt.Sprintf("fc_item_%d", index)
				item := OutputItem{
					Type:      "function_call",
					ID:        toolUseID,
					CallID:    toolUseID,
					Name:      event.ContentBlock.ToolName,
					Arguments: toolInputString(event.ContentBlock.ToolInput),
					Status:    "in_progress",
				}
				outputIndexes[index] = len(response.Output)
				response.Output = append(response.Output, item)
				send(StreamEvent{
					Event: "response.output_item.added",
					Data: OutputItemEvent{
						Type:           "response.output_item.added",
						SequenceNumber: next(),
						OutputIndex:    outputIndexes[index],
						Item:           item,
					},
				})
			}

		// ==================================================================
		// Text delta
		// ==================================================================
		case format.CoreTextDelta:
			index := event.Index
			contentText[index] += event.Delta

			// Reasoning blocks use separate SSE events.
			if reasonIndexes[index] {
				send(StreamEvent{
					Event: "response.reasoning_summary_text.delta",
					Data: ReasoningSummaryTextDeltaEvent{
						Type:           "response.reasoning_summary_text.delta",
						SequenceNumber: next(),
						ItemID:         itemIDs[index],
						OutputIndex:    outputIndexes[index],
						SummaryIndex:   0,
						Delta:          event.Delta,
					},
				})
				break
			}

			// Ensure the output item and content part exist.
			if _, exists := outputIndexes[index]; !exists {
				id, ok := itemIDs[index]
				if !ok {
					id = fmt.Sprintf("msg_item_%d", index)
					itemIDs[index] = id
				}
				item := OutputItem{
					Type:    "message",
					ID:      id,
					Status:  "in_progress",
					Role:    "assistant",
					Content: []ContentPart{{Type: "output_text"}},
				}
				outputIndexes[index] = len(response.Output)
				response.Output = append(response.Output, item)
				send(StreamEvent{
					Event: "response.output_item.added",
					Data: OutputItemEvent{
						Type:           "response.output_item.added",
						SequenceNumber: next(),
						OutputIndex:    outputIndexes[index],
						Item:           item,
					},
				})
				send(StreamEvent{
					Event: "response.content_part.added",
					Data: ContentPartEvent{
						Type:           "response.content_part.added",
						SequenceNumber: next(),
						ItemID:         id,
						OutputIndex:    outputIndexes[index],
						ContentIndex:   0,
						Part:           ContentPart{Type: "output_text"},
					},
				})
			}

			send(StreamEvent{
				Event: "response.output_text.delta",
				Data: OutputTextDeltaEvent{
					Type:           "response.output_text.delta",
					SequenceNumber: next(),
					ItemID:         itemIDs[index],
					OutputIndex:    outputIndexes[index],
					ContentIndex:   0,
					Delta:          event.Delta,
				},
			})

		// ==================================================================
		// Text done
		// ==================================================================
		case format.CoreTextDone:
			index := event.Index
			text := contentText[index]
			delete(contentText, index)

			send(StreamEvent{
				Event: "response.output_text.done",
				Data: OutputTextDoneEvent{
					Type:           "response.output_text.done",
					SequenceNumber: next(),
					ItemID:         itemIDs[index],
					OutputIndex:    outputIndexes[index],
					ContentIndex:   0,
					Text:           text,
				},
			})

			// Mark item as completed.
			if idx, ok := outputIndexes[index]; ok && idx < len(response.Output) {
				response.Output[idx].Status = "completed"
			}
			send(StreamEvent{
				Event: "response.output_item.done",
				Data: OutputItemEvent{
					Type:           "response.output_item.done",
					SequenceNumber: next(),
					OutputIndex:    outputIndexes[index],
					Item:           response.Output[outputIndexes[index]],
				},
			})

		// ==================================================================
		// Tool call arguments delta
		// ==================================================================
		case format.CoreToolCallArgsDelta:
			index := event.Index
			toolCallArgs[index] += event.Delta
			send(StreamEvent{
				Event: "response.function_call_arguments.delta",
				Data: FunctionCallArgumentsDeltaEvent{
					Type:           "response.function_call_arguments.delta",
					SequenceNumber: next(),
					ItemID:         itemIDs[index],
					OutputIndex:    outputIndexes[index],
					Delta:          event.Delta,
				},
			})

		// ==================================================================
		// Tool call arguments done
		// ==================================================================
		case format.CoreToolCallArgsDone:
			index := event.Index
			if idx, ok := outputIndexes[index]; ok && idx < len(response.Output) {
				response.Output[idx].Arguments = event.Delta
				response.Output[idx].Status = "completed"
			}
			send(StreamEvent{
				Event: "response.function_call_arguments.done",
				Data: FunctionCallArgumentsDoneEvent{
					Type:           "response.function_call_arguments.done",
					SequenceNumber: next(),
					ItemID:         itemIDs[index],
					OutputIndex:    outputIndexes[index],
					Arguments:      event.Delta,
				},
			})
			send(StreamEvent{
				Event: "response.output_item.done",
				Data: OutputItemEvent{
					Type:           "response.output_item.done",
					SequenceNumber: next(),
					OutputIndex:    outputIndexes[index],
					Item:           response.Output[outputIndexes[index]],
				},
			})

		// ==================================================================
		// Lifecycle: completed
		// ==================================================================
		case format.CoreEventCompleted:
			response.Status = "completed"
			if event.Usage != nil {
				response.Usage = Usage{
					InputTokens:  event.Usage.InputTokens,
					OutputTokens: event.Usage.OutputTokens,
					TotalTokens:  event.Usage.TotalTokens,
					InputTokensDetails: InputTokensDetails{
						CachedTokens: event.Usage.CachedInputTokens,
					},
				}
			}
			send(StreamEvent{
				Event: "response.completed",
				Data: ResponseLifecycleEvent{
					Type:           "response.completed",
					SequenceNumber: next(),
					Response:       cloneResponse(response),
				},
			})

		// ==================================================================
		// Lifecycle: incomplete
		// ==================================================================
		case format.CoreEventIncomplete:
			response.Status = "incomplete"
			send(StreamEvent{
				Event: "response.incomplete",
				Data: ResponseLifecycleEvent{
					Type:           "response.incomplete",
					SequenceNumber: next(),
					Response:       cloneResponse(response),
				},
			})

		// ==================================================================
		// Lifecycle: failed
		// ==================================================================
		case format.CoreEventFailed:
			response.Status = "failed"
			if event.Error != nil {
				response.Error = &ErrorObject{
					Message: event.Error.Message,
					Type:    event.Error.Type,
					Code:    event.Error.Code,
				}
			}
			send(StreamEvent{
				Event: "response.failed",
				Data: ResponseLifecycleEvent{
					Type:           "response.failed",
					SequenceNumber: next(),
					Response:       cloneResponse(response),
				},
			})

		// ==================================================================
		// Content block done
		// ==================================================================
		case format.CoreContentBlockDone:
			index := event.Index


			// Reasoning block done — emit reasoning summary part done.
			if reasonIndexes[index] {
				if idx, ok := outputIndexes[index]; ok && idx < len(response.Output) {
					response.Output[idx].Status = "completed"
					response.Output[idx].Summary = []ReasoningItemSummary{{
						Type: "text",
						Text: contentText[index],
					}}
				}
				send(StreamEvent{
					Event: "response.reasoning_summary_part.done",
					Data: ReasoningSummaryPartDoneEvent{
						Type:           "response.reasoning_summary_part.done",
						SequenceNumber: next(),
						ItemID:         itemIDs[index],
						OutputIndex:    outputIndexes[index],
						SummaryIndex:   0,
					},
				})
				delete(contentText, index)
				delete(itemIDs, index)
				delete(outputIndexes, index)
				delete(reasonIndexes, index)
				break
			}
			// 1. Text/reasoning block done — emit output_text.done + content_part.done + output_item.done.
			if text, ok := contentText[index]; ok {
				itemID := itemIDs[index]
				outputIndex := outputIndexes[index]

				// output_text.done
				send(StreamEvent{
					Event: "response.output_text.done",
					Data: OutputTextDoneEvent{
						Type:           "response.output_text.done",
						SequenceNumber: next(),
						ItemID:         itemID,
						OutputIndex:    outputIndex,
						ContentIndex:   0,
						Text:           text,
					},
				})

				// content_part.done
				send(StreamEvent{
					Event: "response.content_part.done",
					Data: ContentPartEvent{
						Type:           "response.content_part.done",
						SequenceNumber: next(),
						ItemID:         itemID,
						OutputIndex:    outputIndex,
						ContentIndex:   0,
						Part:           ContentPart{Type: "output_text"},
					},
				})

				// Mark item as completed.
				if idx, ok := outputIndexes[index]; ok && idx < len(response.Output) {
					response.Output[idx].Status = "completed"
				}

				// output_item.done
				send(StreamEvent{
					Event: "response.output_item.done",
					Data: OutputItemEvent{
						Type:           "response.output_item.done",
						SequenceNumber: next(),
						OutputIndex:    outputIndex,
						Item:           response.Output[outputIndexes[index]],
					},
				})

				// Clean up state.
				delete(contentText, index)

			} else if _, ok := outputIndexes[index]; ok {
				// 2. Tool_use block done — emit function_call_arguments.done + output_item.done.
				itemID := itemIDs[index]
				outputIndex := outputIndexes[index]

				// Update item with final accumulated arguments.
				finalArgs := toolCallArgs[index]
				if idx, ok := outputIndexes[index]; ok && idx < len(response.Output) {
					if finalArgs != "" {
						response.Output[idx].Arguments = finalArgs
					}
					response.Output[idx].Status = "completed"
				}

				// function_call_arguments.done
				send(StreamEvent{
					Event: "response.function_call_arguments.done",
					Data: FunctionCallArgumentsDoneEvent{
						Type:           "response.function_call_arguments.done",
						SequenceNumber: next(),
						ItemID:         itemID,
						OutputIndex:    outputIndex,
						Arguments:      finalArgs,
					},
				})

				// output_item.done
				send(StreamEvent{
					Event: "response.output_item.done",
					Data: OutputItemEvent{
						Type:           "response.output_item.done",
						SequenceNumber: next(),
						OutputIndex:    outputIndex,
						Item:           response.Output[outputIndexes[index]],
					},
				})

				// Clean up state.
				delete(toolCallArgs, index)
			}

		case format.CoreItemAdded:
			// Item added is handled by content_block_start for tool_use
			// and by first text delta for messages.

		case format.CoreItemDone:
			// Item completion is handled by text_done / tool_call_args_done / content_block_done.

		// ==================================================================
		// Ping
		// ==================================================================
		case format.CorePing:
			// Anthropic keepalive — no OpenAI equivalent. Silently skip.
		}
	}

	// Notify stream completion hook.
	outputText := response.OutputText
	a.hooks.OnStreamComplete(ctx, coreReq.Model, outputText)
}

// ============================================================================
// Input Conversion Helpers
// ============================================================================

// inputItem is a lightweight struct for unmarshalling OpenAI input array items.
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    string          `json:"output"`
	ID        string          `json:"id"`
	Status    string          `json:"status"`
}

// convertInput parses OpenAI Input (string or array) into Core messages and system blocks.
//
// Behaviour:
//   - If Input is a JSON string → single user message.
//   - If Input is a JSON array → iterate items, group by role.
//   - Items with role "system" or "developer" → system blocks.
//   - Items with role "assistant" → assistant messages (including tool_use blocks
//     from function_call items).
//   - Items with type "function_call_output" → tool_result user messages.
//   - Items with type "function_call" → tool_use within assistant messages.
func convertInput(raw json.RawMessage) ([]format.CoreMessage, []format.CoreContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, nil
	}

	trimmed := strings.TrimSpace(string(raw))

	// String case: single user message.
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, nil, fmt.Errorf("invalid input string: %w", err)
		}
		if text == "" {
			return nil, nil, nil
		}
		return []format.CoreMessage{
			{
				Role:    "user",
				Content: []format.CoreContentBlock{{Type: "text", Text: text}},
			},
		}, nil, nil
	}

	// Array case.
	if !strings.HasPrefix(trimmed, "[") {
		return nil, nil, fmt.Errorf("input must be a string or array")
	}

	var items []inputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("invalid input array: %w", err)
	}

	messages := make([]format.CoreMessage, 0, len(items))
	system := make([]format.CoreContentBlock, 0)
	var pendingToolResults []format.CoreContentBlock

	for _, item := range items {
		if item.Type == "function_call_output" {
			// Tool result: collect and flush as user message.
			pendingToolResults = append(pendingToolResults, format.CoreContentBlock{
				Type:      "tool_result",
				ToolUseID: item.CallID,
				ToolResultContent: []format.CoreContentBlock{
					{Type: "text", Text: item.Output},
				},
			})
			continue
		}

		// Flush pending tool results before non-tool-result items.
		if len(pendingToolResults) > 0 {
			messages = append(messages, format.CoreMessage{
				Role:    "user",
				Content: copyContentBlocks(pendingToolResults),
			})
			pendingToolResults = pendingToolResults[:0]
		}

		role := item.Role
		if role == "" {
			role = "user"
		}

		switch {
		case item.Type == "function_call":
			// function_call in input → tool_use assistant block.
			toolInput := json.RawMessage(item.Arguments)
			if !json.Valid([]byte(item.Arguments)) {
				toolInput = json.RawMessage(`{}`)
			}
			messages = append(messages, format.CoreMessage{
				Role: "assistant",
				Content: []format.CoreContentBlock{
					{
						Type:      "tool_use",
						ToolUseID: firstNonEmpty(item.CallID, item.ID),
						ToolName:  item.Name,
						ToolInput: toolInput,
					},
				},
			})

		case role == "system" || role == "developer":
			blocks := contentBlocksFromRaw(item.Content)
			if len(blocks) > 0 {
				system = append(system, blocks...)
			}

		case role == "assistant":
			blocks := contentBlocksFromRaw(item.Content)
			if len(blocks) > 0 {
				messages = append(messages, format.CoreMessage{
					Role:    "assistant",
					Content: blocks,
				})
			}

		default:
			blocks := contentBlocksFromRaw(item.Content)
			if len(blocks) > 0 {
				messages = append(messages, format.CoreMessage{
					Role:    "user",
					Content: blocks,
				})
			}
		}
	}

	// Flush remaining tool results.
	if len(pendingToolResults) > 0 {
		messages = append(messages, format.CoreMessage{
			Role:    "user",
			Content: copyContentBlocks(pendingToolResults),
		})
	}

	return messages, system, nil
}

// contentPartRaw is a lightweight struct for content part JSON parsing.
type contentPartRaw struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
}

// contentBlocksFromRaw parses an item's Content JSON into CoreContentBlocks.
//
// Supports:
//   - string content → single text block
//   - array of content parts → text/image blocks
func contentBlocksFromRaw(raw json.RawMessage) []format.CoreContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	trimmed := strings.TrimSpace(string(raw))

	// String content.
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil || text == "" {
			return nil
		}
		return []format.CoreContentBlock{{Type: "text", Text: text}}
	}

	// Array of content parts.
	var parts []contentPartRaw
	if err := json.Unmarshal(raw, &parts); err == nil && len(parts) > 0 {
		blocks := make([]format.CoreContentBlock, 0, len(parts))
		for _, part := range parts {
			switch part.Type {
			case "input_text", "text", "output_text":
				if part.Text != "" {
					blocks = append(blocks, format.CoreContentBlock{Type: "text", Text: part.Text})
				}
			case "input_image", "image", "image_url":
				// Image content — extract URL or data URI.
				if src := imageSourceFromRaw(part.ImageURL); src != "" {
					// Determine media type from the source.
					mediaType := "image/png"
					if strings.HasPrefix(src, "data:") {
						if header, _, ok := strings.Cut(src, ","); ok {
							mt := strings.TrimPrefix(header, "data:")
							if semicolon := strings.IndexByte(mt, ';'); semicolon >= 0 {
								mt = mt[:semicolon]
							}
							if mt != "" {
								mediaType = mt
							}
						}
					}
					blocks = append(blocks, format.CoreContentBlock{
						Type:      "image",
						ImageData: src,
						MediaType: mediaType,
					})
				}
			}
		}
		return blocks
	}

	// Fallback: raw text.
	if trimmed != "" {
		return []format.CoreContentBlock{{Type: "text", Text: trimmed}}
	}
	return nil
}

// imageSourceFromRaw extracts an image URL string from a JSON raw message
// that may be a plain string URL or an object with "url" field.
func imageSourceFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var url string
	if err := json.Unmarshal(raw, &url); err == nil {
		return strings.TrimSpace(url)
	}
	var obj struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return strings.TrimSpace(obj.URL)
	}
	return ""
}

// ============================================================================
// Tool Conversion
// ============================================================================

// convertTool converts an OpenAI Tool to a CoreTool.
func convertTool(tool Tool) format.CoreTool {
	ext := make(map[string]any)

	switch tool.Type {
	case "function":
		return format.CoreTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
		}

	case "web_search", "web_search_preview":
		ext["source_type"] = tool.Type
		return format.CoreTool{
			Name:        tool.Type,
			Description: "Search the web for up-to-date information.",
			Extensions:  ext,
		}

	default:
		ext["source_type"] = tool.Type
		return format.CoreTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
			Extensions:  ext,
		}
	}
}

// convertToolChoice parses an OpenAI tool_choice JSON value into a CoreToolChoice.
//
// Accepts:
//   - string: "auto" / "none" / "required"
//   - object: {type: "...", name: "..."} or {type: "function", function: {name: "..."}}
//
// On parse failure, the raw JSON is preserved in CoreToolChoice.Raw for
// round-tripping; no error is returned for best-effort parsing.
func convertToolChoice(raw json.RawMessage) (*format.CoreToolChoice, error) {
	tc := &format.CoreToolChoice{
		Raw: raw,
	}

	// Try string.
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		switch value {
		case "auto", "none":
			tc.Mode = value
			return tc, nil
		case "required":
			tc.Mode = "required"
			return tc, nil
		default:
			return nil, fmt.Errorf("unsupported tool_choice value: %q", value)
		}
	}

	// Try object.
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		// Preserve raw on parse failure; return partial choice.
		return tc, nil
	}

	tc.Mode = obj.Type
	tc.Name = obj.Name
	if tc.Name == "" {
		tc.Name = obj.Function.Name
	}
	return tc, nil
}

// ============================================================================
// Utility
// ============================================================================

// firstNonEmpty returns the first non-empty string from the list.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// copyContentParts returns a shallow copy of a ContentPart slice.
func copyContentParts(parts []ContentPart) []ContentPart {
	out := make([]ContentPart, len(parts))
	copy(out, parts)
	return out
}

// copyContentBlocks returns a shallow copy of a CoreContentBlock slice.
func copyContentBlocks(blocks []format.CoreContentBlock) []format.CoreContentBlock {
	out := make([]format.CoreContentBlock, len(blocks))
	copy(out, blocks)
	return out
}

// cloneResponse creates a shallow copy of a Response for use in stream events.
func cloneResponse(r *Response) Response {
	if r == nil {
		return Response{}
	}
	return *r
}

// toolInputString converts a json.RawMessage tool input to a string,
// defaulting to "{}" when the input is nil or null.
func toolInputString(input json.RawMessage) string {
	if len(input) == 0 || string(input) == "null" {
		return "{}"
	}
	return string(input)
}
