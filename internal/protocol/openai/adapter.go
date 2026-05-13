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

	"moonbridge/internal/format"
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

	hooks format.CorePluginHooks

	streamMu     sync.Mutex
	streamEvents []StreamEvent
}

// NewOpenAIAdapter creates a new OpenAIAdapter with the given config and hooks.
func NewOpenAIAdapter(hooks format.CorePluginHooks) *OpenAIAdapter {
	return &OpenAIAdapter{
		hooks: hooks.WithDefaults(),
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
//   - Instructions → prepended to System
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

	// 3. Prepend Instructions to System (highest priority).
	if openaiReq.Instructions != "" {
		system = append([]format.CoreContentBlock{{
			Type: "text",
			Text: openaiReq.Instructions,
		}}, system...)
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

	// 8. OpenAI-specific extension fields.
	openaiExt := make(map[string]any)
	if openaiReq.ParallelToolCalls != nil {
		openaiExt["parallel_tool_calls"] = *openaiReq.ParallelToolCalls
	}
	if len(openaiReq.Include) > 0 {
		openaiExt["include"] = openaiReq.Include
	}
	if len(openaiReq.Reasoning) > 0 {
		openaiExt["reasoning"] = openaiReq.Reasoning
	}
	if len(openaiReq.Text) > 0 {
		openaiExt["text"] = openaiReq.Text
	}
	if openaiReq.ServiceTier != "" {
		openaiExt["service_tier"] = openaiReq.ServiceTier
	}
	if openaiReq.PreviousResponseID != "" {
		openaiExt["previous_response_id"] = openaiReq.PreviousResponseID
	}
	if openaiReq.Store != nil {
		openaiExt["store"] = *openaiReq.Store
	}
	if len(openaiExt) > 0 {
		coreReq.Extensions["openai"] = openaiExt
	}


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
						{Type: "text", Text: block.ReasoningText, Signature: block.ReasoningSignature},
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
					Arguments: normalizePatchInput(block.ToolName, toolInputString(block.ToolInput)),
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
	usage := Usage{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		TotalTokens:  resp.Usage.TotalTokens,
		InputTokensDetails: InputTokensDetails{
			CachedTokens: resp.Usage.CachedInputTokens,
		},
	}
	// Extract OutputTokensDetails from extensions if available.
	if resp.Extensions != nil {
		if otRaw, ok := resp.Extensions["output_tokens_details"]; ok {
			if otMap, ok := otRaw.(map[string]any); ok {
				if rt, ok := otMap["reasoning_tokens"]; ok {
					if rtVal, ok := rt.(float64); ok {
						usage.OutputTokensDetails = OutputTokensDetails{
							ReasoningTokens: int(rtVal),
						}
					}
				}
			}
		}
	}
	response.Usage = usage


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

	// Reset stream event buffer for this request.
	a.streamMu.Lock()
	a.streamEvents = nil
	a.streamMu.Unlock()

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

			blockType := event.ContentBlock.Type
			if blockType == "reasoning" && !hasReasoningRequested(coreReq) {
				blockType = "text"
			}

			switch blockType {
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
					Type:    "reasoning",
					ID:      id,
					Status:  "in_progress",
					Summary: []ReasoningItemSummary{},
				})
				send(StreamEvent{
					Event: "response.output_item.added",
					Data: OutputItemEvent{
						Type:           "response.output_item.added",
						SequenceNumber: next(),
						OutputIndex:    io,
						Item:           response.Output[io],
					},
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
					Arguments: normalizePatchInput(event.ContentBlock.ToolName, toolInputString(event.ContentBlock.ToolInput)),
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

			// Ensure output item exists (may not be created if no deltas arrived).
			if _, hasOutput := outputIndexes[index]; !hasOutput {
				id := itemIDs[index]
				if id == "" {
					id = fmt.Sprintf("msg_item_%d", index)
					itemIDs[index] = id
				}
				outputIndexes[index] = len(response.Output)
				response.Output = append(response.Output, OutputItem{
					Type:    "message",
					ID:      id,
					Status:  "in_progress",
					Role:    "assistant",
					Content: []ContentPart{{Type: "output_text"}},
				})
			}

			outIdx := outputIndexes[index]

			// Store accumulated text in response output for final completed event.
			if outIdx < len(response.Output) && len(response.Output[outIdx].Content) > 0 {
				response.Output[outIdx].Content[0].Text = text
			}

			send(StreamEvent{
				Event: "response.output_text.done",
				Data: OutputTextDoneEvent{
					Type:           "response.output_text.done",
					SequenceNumber: next(),
					ItemID:         itemIDs[index],
					OutputIndex:    outIdx,
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
					OutputIndex:    outIdx,
					Item:           response.Output[outIdx],
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
			// Build output_text from message items, same as FromCoreResponse.
			var texts []string
			for _, item := range response.Output {
				if item.Type == "message" {
					for _, part := range item.Content {
						if part.Type == "output_text" || part.Type == "text" {
							texts = append(texts, part.Text)
						}
					}
				}
			}
			response.OutputText = strings.Join(texts, "")
			response.Status = "completed"
			if event.Usage != nil {
				response.Usage = Usage{
					InputTokens:  event.Usage.InputTokens,
					OutputTokens: event.Usage.OutputTokens,
					TotalTokens:  event.Usage.InputTokens + event.Usage.OutputTokens,
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
					sig := ""
					if event.ContentBlock != nil {
						sig = event.ContentBlock.ReasoningSignature
					}
					response.Output[idx].Summary = []ReasoningItemSummary{{
						Type:      "text",
						Text:      contentText[index],
						Signature: sig,
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
				// Ensure output item exists (may not be created if no deltas arrived).
				if _, hasOutput := outputIndexes[index]; !hasOutput {
					if itemID == "" {
						itemID = fmt.Sprintf("msg_item_%d", index)
						itemIDs[index] = itemID
					}
					outputIndexes[index] = len(response.Output)
					response.Output = append(response.Output, OutputItem{
						Type:    "message",
						ID:      itemID,
						Status:  "in_progress",
						Role:    "assistant",
						Content: []ContentPart{{Type: "output_text"}},
					})
				}
				outputIndex := outputIndexes[index]

				// Store accumulated text in response output for final completed event.
				if outputIndex < len(response.Output) && len(response.Output[outputIndex].Content) > 0 {
					response.Output[outputIndex].Content[0].Text = text
				}

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

// hasReasoningRequested checks whether the original request asked for reasoning.
// DeepSeek V4 always returns thinking blocks regardless, but reasoning_summary
// events should only be emitted when the client explicitly requested reasoning.
// When reasoning wasn't requested, thinking blocks are treated as regular text
// to avoid "ReasoningSummaryDelta without active item" errors in Codex.
//
// It checks three sources:
//
//  1. Output.Effort — OpenAI-native reasoning effort (used by o3/o4-mini).
//     True when the field is set to a non-empty, non-"none" value.
//
//  2. Thinking.Type — Anthropic/Claude extended thinking.
//     True when Thinking is present and Type == "enabled".
//
//  3. Extensions["openai"]["reasoning"] — generic passthrough for provider-specific
//     reasoning configuration injected via request extensions.
func hasReasoningRequested(coreReq *format.CoreRequest) bool {
	if coreReq.Output != nil && coreReq.Output.Effort != "" && coreReq.Output.Effort != "none" {
		return true
	}
	if coreReq.Thinking != nil && coreReq.Thinking.Type == "enabled" {
		return true
	}
	if openaiExt, ok := coreReq.Extensions["openai"].(map[string]any); ok {
		if _, ok := openaiExt["reasoning"]; ok {
			return true
		}
	}
	return false
}

// ============================================================================
// Input Conversion Helpers
// ============================================================================

// inputItem is a lightweight struct for unmarshalling OpenAI input array items.
type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
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
	var pendingReasoning []format.CoreContentBlock
	var pendingFCBlocks []format.CoreContentBlock // batch consecutive function_calls

	for _, item := range items {
		if item.Type == "function_call_output" {
			// Merge pending reasoning into pendingFCBlocks so they become
			// a single assistant message. This keeps tool_use and tool_result
			// adjacent, satisfying the Anthropic protocol requirement that
			// every tool_use must be immediately followed by a tool_result
			// in the next message.
			if len(pendingReasoning) > 0 {
				pendingFCBlocks = append(pendingFCBlocks, pendingReasoning...)
				pendingReasoning = pendingReasoning[:0]
			}
			// Flush pending function_calls before tool results.
			if len(pendingFCBlocks) > 0 {
				flushed := make([]format.CoreContentBlock, len(pendingFCBlocks))
				copy(flushed, pendingFCBlocks)
				messages = append(messages, format.CoreMessage{
					Role:    "assistant",
					Content: flushed,
				})
				pendingFCBlocks = pendingFCBlocks[:0]
			}
			// Each tool result → separate tool-role Core message.
			messages = append(messages, format.CoreMessage{
				Role: "tool",
				Content: []format.CoreContentBlock{{
					Type:      "tool_result",
					ToolUseID: item.CallID,
					ToolResultContent: []format.CoreContentBlock{
						{Type: "text", Text: outputToString(item.Output)},
					},
				}},
			})
			continue
		}
		// Flush pending function_calls before non-function-call items.
		// Don't flush between consecutive function_call items — they should
		// be batched into a single assistant message.
		if item.Type != "function_call" && len(pendingFCBlocks) > 0 {
			flushed := make([]format.CoreContentBlock, len(pendingFCBlocks))
			copy(flushed, pendingFCBlocks)
			messages = append(messages, format.CoreMessage{
				Role:    "assistant",
				Content: flushed,
			})
			pendingFCBlocks = pendingFCBlocks[:0]
		}

		role := item.Role
		if role == "" {
			role = "user"
		}

		// Handle reasoning input items — convert to thinking blocks for the next assistant message.
		if item.Type == "reasoning" {
			blocks := reasoningBlocksFromSummary(item.Summary)
			pendingReasoning = append(pendingReasoning, blocks...)
			continue
		}

		switch {
		case item.Type == "function_call":
		// NOTE: Reasoning alignment for inference models (o3/o4-mini):
		// OpenAI requires a "reasoning" input item before each "function_call" item
		// when using reasoning models. Currently, pendingReasoning blocks (from preceding
		// reasoning input items) are merged into the next assistant message, but no
		// dummy reasoning block is injected if pendingReasoning is empty.
		// A future fix should add a dummy reasoning block here when:
		// (a) the model is a reasoning model, and (b) pendingReasoning is empty.

			// function_call in input → tool_use assistant block.
			// Collect into pendingFCBlocks to batch consecutive calls into a single assistant message.
			if len(pendingReasoning) > 0 {
				pendingFCBlocks = append(pendingFCBlocks, pendingReasoning...)
				pendingReasoning = pendingReasoning[:0]
			}
			toolInput := json.RawMessage(item.Arguments)
			if !json.Valid([]byte(item.Arguments)) {
				toolInput = json.RawMessage(`{}`)
			}
			pendingFCBlocks = append(pendingFCBlocks, format.CoreContentBlock{
				Type:      "tool_use",
				ToolUseID: firstNonEmpty(item.CallID, item.ID),
				ToolName:  item.Name,
				ToolInput: toolInput,
			})

		case role == "system" || role == "developer":
			blocks := contentBlocksFromRaw(item.Content)
			if len(blocks) > 0 {
				system = append(system, blocks...)
			}

		case role == "assistant":
			blocks := contentBlocksFromRaw(item.Content)
			// Prepend any pending reasoning blocks (from previous reasoning input items)
			// before the assistant message content.
			if len(pendingReasoning) > 0 {
				blocks = append(pendingReasoning, blocks...)
				pendingReasoning = pendingReasoning[:0]
			}
			if len(pendingFCBlocks) > 0 {
				// Merge into pendingFCBlocks to keep tool_use + text in one
				// assistant message. This preserves tool_use/tool_result adjacency
				// required by the Anthropic protocol.
				pendingFCBlocks = append(pendingFCBlocks, blocks...)
			} else if len(blocks) > 0 {
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

	// Flush remaining reasoning blocks (no following assistant message).
	if len(pendingReasoning) > 0 {
		messages = append(messages, format.CoreMessage{
			Role:    "assistant",
			Content: pendingReasoning,
		})
	}

	// Flush any remaining batched function_calls.
	if len(pendingFCBlocks) > 0 {
		flushed := make([]format.CoreContentBlock, len(pendingFCBlocks))
		copy(flushed, pendingFCBlocks)
		messages = append(messages, format.CoreMessage{
			Role:    "assistant",
			Content: flushed,
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
	case "file_search":
		ext["source_type"] = tool.Type
		ext["max_num_results"] = tool.MaxNumResults
		return format.CoreTool{
			Name:        tool.Type,
			Description: "Search files in the user's file system.",
			Extensions:  ext,
		}

	case "code_interpreter":
		ext["source_type"] = tool.Type
		return format.CoreTool{
			Name:        tool.Type,
			Description: "Execute code in a sandboxed interpreter.",
			Extensions:  ext,
		}

	case "computer_use_preview":
		ext["source_type"] = tool.Type
		ext["display_width"] = tool.DisplayWidth
		ext["display_height"] = tool.DisplayHeight
		return format.CoreTool{
			Name:        tool.Type,
			Description: "Use a computer to perform actions.",
			Extensions:  ext,
		}


	default:
		ext["source_type"] = tool.Type
		return format.CoreTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: adaptSchema(tool.Parameters, tool.Name),
			Extensions:  ext,
		}
	}
}

// adaptSchema ensures custom Codex CLI tools have a proper input schema,
// since models like DeepSeek/Claude don't natively understand the Codex
// custom tool format (type: "custom").
func adaptSchema(params map[string]any, toolName string) map[string]any {
	if params != nil && len(params) > 0 {
		// Has existing schema, just clean it
		return params
	}
	// Generate schemas for known Codex custom tools
	switch toolName {
	case "apply_patch", "patch":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string", "description": "The patch content or operation to apply"},
			},
			"required": []string{"input"},
		}
	case "exec", "exec_command", "local_shell", "shell":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
					"description": "Command and arguments to execute",
				},
				"description": map[string]any{"type": "string"},
			},
			"required": []string{"command"},
		}
	case "read", "view", "view_image":
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string"},
			},
			"required": []string{"file_path"},
		}
	default:
		return map[string]any{"type": "object"}
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

// reasoningSummaryItem is a lightweight struct for unmarshalling reasoning summary JSON.
type reasoningSummaryItem struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

// reasoningBlocksFromSummary parses a reasoning summary JSON array and converts
// each item to a CoreContentBlock of type "reasoning".
// This preserves the thinking text and optional signature for upstream replay.
func reasoningBlocksFromSummary(raw json.RawMessage) []format.CoreContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []reasoningSummaryItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	blocks := make([]format.CoreContentBlock, 0, len(items))
	for _, item := range items {
		if item.Text == "" {
			continue
		}
		blocks = append(blocks, format.CoreContentBlock{
			Type:          "reasoning",
			ReasoningText: item.Text,
			// Use Signature from the item if present (adapter-created "text" type).
			// This preserves the provider-specific thinking signature needed for
			// continuing reasoning chains across conversation turns.
			ReasoningSignature: item.Signature,
		})
	}
	return blocks
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

// normalizePatchInput normalizes apply_patch tool input from various DeepSeek-generated
// patch formats into Codex's expected *** Begin Patch / *** End Patch format.
//
// DeepSeek sometimes generates non-standard patch markers (*** Add File, *** Modify File)
// or bare unified diffs without the Begin/End Patch delimiters. This normalizes them
// deterministically so the agent doesn't need to fall back to heredoc.
func normalizePatchInput(toolName, input string) string {
	lower := strings.ToLower(toolName)
	if lower != "apply_patch" && lower != "patch" {
		return input
	}

	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed == "{}" {
		return input
	}

	// If input is a JSON object with an "input" field, normalize the inner value.
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			if patchStr, ok := obj["input"].(string); ok && patchStr != "" {
				normalized := normalizePatchContent(patchStr)
				if normalized == patchStr {
					return input // unchanged
				}
				obj["input"] = normalized
				if result, err := json.Marshal(obj); err == nil {
					return string(result)
				}
			}
		}
		return input
	}

	// Plain string content — normalize directly.
	return normalizePatchContent(trimmed)
}

// normalizePatchContent normalizes a raw patch string to *** Begin Patch / *** End Patch format.
func normalizePatchContent(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return content
	}

	// Already in the expected format.
	if strings.Contains(trimmed, "*** Begin Patch") {
		return trimmed
	}

	hadNonStandardMarker := false

	// Strip non-standard *** markers (Add File, Modify File, Delete File).
	// These are DeepSeek inventions that Codex doesn't recognize.
	for _, marker := range []string{"*** Add File", "*** Modify File", "*** Delete File", "*** Update File"} {
		if strings.HasPrefix(trimmed, marker) {
			hadNonStandardMarker = true
			// Remove the marker and any path line that follows it.
			trimmed = strings.TrimPrefix(trimmed, marker)
			trimmed = strings.TrimSpace(trimmed)
			// If the next line looks like a file path (no diff markers), skip it.
			if idx := strings.Index(trimmed, "\n"); idx >= 0 {
				firstLine := strings.TrimSpace(trimmed[:idx])
				if !strings.HasPrefix(firstLine, "---") && !strings.HasPrefix(firstLine, "+++") &&
					!strings.HasPrefix(firstLine, "@@") && !strings.HasPrefix(firstLine, "diff ") {
					trimmed = strings.TrimSpace(trimmed[idx+1:])
				}
			}
			break
		}
	}

	// If we stripped a non-standard marker, we know this is patch content — always wrap.
	if hadNonStandardMarker {
		return "*** Begin Patch\n" + trimmed + "\n*** End Patch"
	}

	// Detect if this looks like a diff/patch.
	looksLikePatch := false
	for _, pattern := range []string{"\n--- ", "\n+++ ", "\n@@ ", "diff --git "} {
		if strings.Contains(trimmed, pattern) {
			looksLikePatch = true
			break
		}
	}
	if strings.HasPrefix(trimmed, "--- ") || strings.HasPrefix(trimmed, "+++ ") ||
		strings.HasPrefix(trimmed, "@@ ") || strings.HasPrefix(trimmed, "diff --git ") {
		looksLikePatch = true
	}

	if looksLikePatch {
		return "*** Begin Patch\n" + trimmed + "\n*** End Patch"
	}

	// Not a recognizable patch format; pass through unchanged.
	return content
}

// outputToString converts a json.RawMessage Output field to a string.
// The output can be a plain string or an array of content parts (multi-modal).
func outputToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Fallback: return the raw JSON as a string (array/object).
	return string(raw)
}
