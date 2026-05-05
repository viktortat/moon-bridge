package anthropic

import (
	"context"
	"fmt"
	"io"

	"moonbridge/internal/foundation/config"
	"moonbridge/internal/protocol/format"
)

// ---------------------------------------------------------------------------
// CacheManager — local interface to avoid import cycle
// ---------------------------------------------------------------------------
// The cache package imports anthropic (breakpoint.go, planner_ext.go), so
// anthropic cannot import cache.  This interface lets the dispatch layer
// inject cache planning/registry logic without a direct dependency.
//
// Implementations live outside the anthropic package (e.g. in the server
// dispatch or an adapter wiring layer) and wrap the real cache package.

// CacheManager handles prompt cache planning, injection, and registry
// updates for Anthropic requests.
type CacheManager interface {
	// PlanAndInject plans cache breakpoints and injects cache_control
	// into the anthropic MessageRequest.  Returns a stable key + TTL
	// that can be used later to update the registry.
	PlanAndInject(ctx context.Context, req *MessageRequest, coreReq *format.CoreRequest) (key, ttl string)

	// UpdateRegistry updates the in-memory cache registry from upstream
	// usage signals after a response is received.
	UpdateRegistry(ctx context.Context, key, ttl string, usage Usage)
}

// ---------------------------------------------------------------------------
// AnthropicProviderAdapter — implements format.ProviderAdapter + format.ProviderStreamAdapter
// ---------------------------------------------------------------------------

// AnthropicProviderAdapter converts Core format requests/responses to/from
// the Anthropic Messages API format.
//
// Clean room: no dependency on internal/protocol/bridge/.
// Only references: config, format, and anthropic types.
type AnthropicProviderAdapter struct {
	cfg          config.Config
	cacheMgr     CacheManager
	hooks        format.CorePluginHooks
}

// NewAnthropicProviderAdapter creates a new AnthropicProviderAdapter.
//
// cacheMgr handles prompt cache planning.  Pass a no-op implementation
// if caching is not needed.
func NewAnthropicProviderAdapter(cfg config.Config, cacheMgr CacheManager, hooks format.CorePluginHooks) *AnthropicProviderAdapter {
	return &AnthropicProviderAdapter{
		cfg:      cfg,
		cacheMgr: cacheMgr,
		hooks:    hooks.WithDefaults(),
	}
}

// ProviderProtocol returns "anthropic".
func (a *AnthropicProviderAdapter) ProviderProtocol() string {
	return "anthropic"
}

// =========================================================================
// FromCoreRequest — CoreRequest → *anthropic.MessageRequest
// =========================================================================

// FromCoreRequest converts a CoreRequest into an *anthropic.MessageRequest.
//
// Conversion steps:
//  1. Call hooks.MutateCoreRequest (plugin modifications to CoreRequest)
//  2. Map all CoreRequest fields to anthropic.MessageRequest fields
//  3. Cache planning via CacheManager (PlanAndInject)
func (a *AnthropicProviderAdapter) FromCoreRequest(ctx context.Context, req *format.CoreRequest) (any, error) {
	if req == nil {
		return nil, fmt.Errorf("anthropic adapter: core request is nil")
	}

	// Step 1: Allow plugins to mutate the CoreRequest before conversion.
	a.hooks.MutateCoreRequest(ctx, req)

	// Step 2: Build the anthropic MessageRequest.
	anthropicReq := MessageRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Messages:      make([]Message, 0, len(req.Messages)),
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.StopSequences,
		Stream:        req.Stream,
		Metadata:      req.Metadata,
	}

	// System
	if len(req.System) > 0 {
		anthropicReq.System = a.toContentBlocks(req.System)
	}

	// Messages
	for _, msg := range req.Messages {
		anthropicReq.Messages = append(anthropicReq.Messages, Message{
			Role:    msg.Role,
			Content: a.toContentBlocks(msg.Content),
		})
	}

	// Tools
	if len(req.Tools) > 0 {
		anthropicReq.Tools = make([]Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			anthropicReq.Tools = append(anthropicReq.Tools, Tool{
				Name:        t.Name,
				Type:        "custom",
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}
	}

	// ToolChoice
	if req.ToolChoice != nil {
		anthropicReq.ToolChoice = a.toAnthropicToolChoice(*req.ToolChoice)
	}

	// Step 3: Cache planning via CacheManager.
	// PlanAndInject may modify anthropicReq in-place by setting cache_control
	// on tools, system blocks, messages, or the request-level field.
	a.cacheMgr.PlanAndInject(ctx, &anthropicReq, req)

	return &anthropicReq, nil
}

// =========================================================================
// ToCoreResponse — *anthropic.MessageResponse → *format.CoreResponse
// =========================================================================

// ToCoreResponse converts an *anthropic.MessageResponse into a *format.CoreResponse.
//
// The response content blocks become a single assistant message. Cache registry
// is updated from usage signals via CacheManager.
func (a *AnthropicProviderAdapter) ToCoreResponse(ctx context.Context, resp any) (*format.CoreResponse, error) {
	msgResp, ok := resp.(*MessageResponse)
	if !ok {
		return nil, fmt.Errorf("anthropic adapter: expected *anthropic.MessageResponse, got %T", resp)
	}

	// Map stop_reason to Core status.
	status := a.mapStopReasonToStatus(msgResp.StopReason)

	// Convert content blocks to Core message.
	coreContent := a.fromContentBlocks(msgResp.Content)

	coreResp := &format.CoreResponse{
		ID:     msgResp.ID,
		Status: status,
		Model:  msgResp.Model,
		Messages: []format.CoreMessage{
			{
				Role:    "assistant",
				Content: coreContent,
			},
		},
		Usage:      a.toCoreUsage(msgResp.Usage),
		StopReason: msgResp.StopReason,
	}

	// Map error-like stop reasons.
	if msgResp.StopReason == "content_filtered" {
		coreResp.Error = &format.CoreError{
			Type:    "content_filter",
			Message: "response filtered by content moderation",
		}
		coreResp.Status = "failed"
	}

	// Update cache registry from usage signals via CacheManager.
	// The key/ttl were computed during PlanAndInject and must be accessible
	// through the CacheManager's own state (or context).
	if a.cacheMgr != nil {
		a.cacheMgr.UpdateRegistry(ctx, "", "", msgResp.Usage)
	}

	return coreResp, nil
}

// =========================================================================
// ToCoreStream — anthropic.Stream → <-chan format.CoreStreamEvent
// =========================================================================

// streamConverterState tracks state across a stream conversion.
type streamConverterState struct {
	seqNum     int64
	msgID      string
	model      string
	blockTypes map[int]string // content index → block type
}

// ToCoreStream consumes an anthropic.Stream and returns a channel of CoreStreamEvent.
//
// The adapter owns the read-loop goroutine. The returned channel is closed when
// the stream ends, context is cancelled, or an error occurs.
func (a *AnthropicProviderAdapter) ToCoreStream(ctx context.Context, src any) (<-chan format.CoreStreamEvent, error) {
	stream, ok := src.(Stream)
	if !ok {
		return nil, fmt.Errorf("anthropic adapter: expected anthropic.Stream, got %T", src)
	}
	events := make(chan format.CoreStreamEvent, 64)

	go func() {
		defer close(events)
		defer stream.Close()

		state := &streamConverterState{
			blockTypes: make(map[int]string),
		}

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			ev, err := stream.Next()
			if err != nil {
				if err == io.EOF {
					// Stream ended normally.
					return
				}
				// Context cancellation is clean shutdown, not a failure.
				if err == context.Canceled || err == context.DeadlineExceeded {
					return
				}
				state.emit(events, format.CoreStreamEvent{
					Type: format.CoreEventFailed,
					Error: &format.CoreError{
						Message: err.Error(),
					},
				})
				return
			}

			state.convertEvent(events, ev)
		}
	}()

	return events, nil
}

// =========================================================================
// Stream event conversion
// =========================================================================

func (s *streamConverterState) nextSeq() int64 {
	s.seqNum++
	return s.seqNum
}

func (s *streamConverterState) emit(events chan<- format.CoreStreamEvent, ev format.CoreStreamEvent) {
	ev.SeqNum = s.nextSeq()
	events <- ev
}

func (s *streamConverterState) convertEvent(events chan<- format.CoreStreamEvent, ev StreamEvent) {
	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			s.msgID = ev.Message.ID
			s.model = ev.Message.Model

			s.emit(events, format.CoreStreamEvent{
				Type:   format.CoreEventCreated,
				Status: "in_progress",
				Model:  s.model,
			})
			s.emit(events, format.CoreStreamEvent{
				Type:   format.CoreItemAdded,
				ItemID: s.msgID,
			})
		}

	case "content_block_start":
		if ev.ContentBlock == nil {
			return
		}
		index := ev.Index
		blockType := ev.ContentBlock.Type
		s.blockTypes[index] = blockType

		switch blockType {
		case "text":
			s.emit(events, format.CoreStreamEvent{
				Type:  format.CoreContentBlockStarted,
				Index: index,
				ContentBlock: &format.CoreContentBlock{
					Type: "text",
				},
			})

		case "tool_use":
			s.emit(events, format.CoreStreamEvent{
				Type:  format.CoreContentBlockStarted,
				Index: index,
				ContentBlock: &format.CoreContentBlock{
					Type:      "tool_use",
					ToolUseID: ev.ContentBlock.ID,
					ToolName:  ev.ContentBlock.Name,
				},
			})
			s.emit(events, format.CoreStreamEvent{
				Type:   format.CoreItemAdded,
				ItemID: ev.ContentBlock.ID,
			})

		case "thinking":
			s.emit(events, format.CoreStreamEvent{
				Type:  format.CoreContentBlockStarted,
				Index: index,
				ContentBlock: &format.CoreContentBlock{
					Type:               "reasoning",
					ReasoningText:      ev.ContentBlock.Thinking,
					ReasoningSignature: ev.ContentBlock.Signature,
				},
			})
		}

	case "content_block_delta":
		index := ev.Index
		blockType := s.blockTypes[index]

		switch {
		case ev.Delta.Type == "text_delta" || blockType == "text":
			s.emit(events, format.CoreStreamEvent{
				Type:  format.CoreTextDelta,
				Index: index,
				Delta: ev.Delta.Text,
			})

		case ev.Delta.Type == "input_json_delta" || blockType == "tool_use":
			s.emit(events, format.CoreStreamEvent{
				Type:  format.CoreToolCallArgsDelta,
				Index: index,
				Delta: ev.Delta.PartialJSON,
			})

		case ev.Delta.Type == "thinking_delta" || blockType == "thinking":
			s.emit(events, format.CoreStreamEvent{
				Type:  format.CoreTextDelta,
				Index: index,
				Delta: ev.Delta.Thinking,
				ContentBlock: &format.CoreContentBlock{
					Type:               "reasoning",
					ReasoningSignature: ev.Delta.Signature,
				},
			})
		}

	case "content_block_stop":
		index := ev.Index

		s.emit(events, format.CoreStreamEvent{
			Type:  format.CoreContentBlockDone,
			Index: index,
		})
		s.emit(events, format.CoreStreamEvent{
			Type: format.CoreItemDone,
		})

	case "message_delta":
		if ev.Usage != nil {
			s.emit(events, format.CoreStreamEvent{
				Type: format.CoreEventInProgress,
				Usage: &format.CoreUsage{
					InputTokens:       ev.Usage.InputTokens,
					OutputTokens:      ev.Usage.OutputTokens,
					CachedInputTokens: ev.Usage.CacheReadInputTokens,
				},
				StopReason: ev.Delta.StopReason,
			})
		}

	case "message_stop":
		s.emit(events, format.CoreStreamEvent{
			Type:   format.CoreEventCompleted,
			Status: "completed",
			Model:  s.model,
		})

	case "error":
		errMsg := "unknown error"
		errType := "api_error"
		if ev.Error != nil {
			errMsg = ev.Error.Message
			errType = ev.Error.Type
		}
		s.emit(events, format.CoreStreamEvent{
			Type: format.CoreEventFailed,
			Error: &format.CoreError{
				Message: errMsg,
				Type:    errType,
			},
		})

	case "ping":
		s.emit(events, format.CoreStreamEvent{
			Type: format.CorePing,
		})
	}
}

// =========================================================================
// Helpers: Core → Anthropic
// =========================================================================

// toContentBlocks converts []CoreContentBlock to []ContentBlock.
func (a *AnthropicProviderAdapter) toContentBlocks(blocks []format.CoreContentBlock) []ContentBlock {
	result := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		result = append(result, a.toContentBlock(b))
	}
	return result
}

// toContentBlock converts a single CoreContentBlock to anthropic ContentBlock.
func (a *AnthropicProviderAdapter) toContentBlock(b format.CoreContentBlock) ContentBlock {
	switch b.Type {
	case "text":
		block := ContentBlock{
			Type: "text",
			Text: b.Text,
		}
		if cc := a.extractCacheControl(b.Extensions); cc != nil {
			block.CacheControl = cc
		}
		return block

	case "image":
		return ContentBlock{
			Type: "image",
			Source: &ImageSource{
				Type:      "base64",
				Data:      b.ImageData,
				MediaType: b.MediaType,
			},
		}

	case "tool_use":
		return ContentBlock{
			Type:  "tool_use",
			ID:    b.ToolUseID,
			Name:  b.ToolName,
			Input: b.ToolInput,
		}

	case "tool_result":
		var content any
		if len(b.ToolResultContent) > 0 {
			content = a.toContentBlocks(b.ToolResultContent)
		}
		return ContentBlock{
			Type:      "tool_result",
			ToolUseID: b.ToolUseID,
			Content:   content,
		}

	case "reasoning":
		block := ContentBlock{
			Type:      "thinking",
			Thinking:  b.ReasoningText,
			Signature: b.ReasoningSignature,
		}
		if cc := a.extractCacheControl(b.Extensions); cc != nil {
			block.CacheControl = cc
		}
		return block

	default:
		// Fallback: treat unknown types as text.
		return ContentBlock{
			Type: "text",
			Text: b.Text,
		}
	}
}

// toAnthropicToolChoice converts CoreToolChoice to anthropic ToolChoice.
func (a *AnthropicProviderAdapter) toAnthropicToolChoice(tc format.CoreToolChoice) ToolChoice {
	switch tc.Mode {
	case "none":
		return ToolChoice{Type: "none"}
	case "auto":
		return ToolChoice{Type: "auto"}
	case "any", "required":
		if tc.Name != "" {
			return ToolChoice{Type: "tool", Name: tc.Name}
		}
		return ToolChoice{Type: "any"}
	default:
		if tc.Name != "" {
			return ToolChoice{Type: "tool", Name: tc.Name}
		}
		return ToolChoice{Type: "auto"}
	}
}

// extractCacheControl reads cache_control from a CoreContentBlock.Extensions map.
func (a *AnthropicProviderAdapter) extractCacheControl(ext map[string]any) *CacheControl {
	if ext == nil {
		return nil
	}
	raw, ok := ext["cache_control"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case map[string]any:
		ttl, _ := v["ttl"].(string)
		if ctype, ok := v["type"].(string); ok && ctype == "ephemeral" {
			return &CacheControl{Type: "ephemeral", TTL: ttl}
		}
		return nil
	case string:
		if v == "ephemeral" {
			return &CacheControl{Type: "ephemeral"}
		}
		return nil
	default:
		return nil
	}
}

// =========================================================================
// Helpers: Anthropic → Core
// =========================================================================

// fromContentBlocks converts []anthropic.ContentBlock to []CoreContentBlock.
func (a *AnthropicProviderAdapter) fromContentBlocks(blocks []ContentBlock) []format.CoreContentBlock {
	result := make([]format.CoreContentBlock, 0, len(blocks))
	for _, b := range blocks {
		result = append(result, a.fromContentBlock(b))
	}
	return result
}

// fromContentBlock converts a single anthropic ContentBlock to CoreContentBlock.
func (a *AnthropicProviderAdapter) fromContentBlock(b ContentBlock) format.CoreContentBlock {
	switch b.Type {
	case "text":
		return format.CoreContentBlock{
			Type: "text",
			Text: b.Text,
		}

	case "image":
		cb := format.CoreContentBlock{
			Type: "image",
		}
		if b.Source != nil {
			cb.MediaType = b.Source.MediaType
			cb.ImageData = b.Source.Data
		}
		return cb

	case "tool_use":
		return format.CoreContentBlock{
			Type:      "tool_use",
			ToolUseID: b.ID,
			ToolName:  b.Name,
			ToolInput: b.Input,
		}

	case "tool_result":
		cb := format.CoreContentBlock{
			Type:      "tool_result",
			ToolUseID: b.ToolUseID,
		}
		if b.Content != nil {
			switch content := b.Content.(type) {
			case string:
				cb.ToolResultContent = []format.CoreContentBlock{
					{Type: "text", Text: content},
				}
			case []ContentBlock:
				cb.ToolResultContent = a.fromContentBlocks(content)
			}
		}
		return cb

	case "thinking":
		return format.CoreContentBlock{
			Type:               "reasoning",
			ReasoningText:      b.Thinking,
			ReasoningSignature: b.Signature,
		}

	default:
		if b.Text != "" {
			return format.CoreContentBlock{
				Type: "text",
				Text: b.Text,
			}
		}
		return format.CoreContentBlock{
			Type: "text",
		}
	}
}

// toCoreUsage converts anthropic Usage to CoreUsage.
func (a *AnthropicProviderAdapter) toCoreUsage(u Usage) format.CoreUsage {
	cached := u.CacheReadInputTokens
	if cached == 0 {
		cached = u.CacheCreationInputTokens
	}
	return format.CoreUsage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		TotalTokens:       u.InputTokens + u.OutputTokens,
		CachedInputTokens: cached,
	}
}

// mapStopReasonToStatus maps anthropic stop_reason to Core status string.
func (a *AnthropicProviderAdapter) mapStopReasonToStatus(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "tool_use":
		return "completed"
	case "max_tokens":
		return "incomplete"
	case "content_filtered":
		return "failed"
	case "":
		return "in_progress"
	default:
		return "completed"
	}
}
