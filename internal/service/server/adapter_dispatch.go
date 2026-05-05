package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"moonbridge/internal/foundation/config"
	openai "moonbridge/internal/protocol/openai"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/cache"
	"moonbridge/internal/protocol/format"
	"moonbridge/internal/service/stats"
	"moonbridge/internal/service/provider"
	mbtrace "moonbridge/internal/service/trace"
)

// ============================================================================
// Adapter Dispatch — experimental dual-bridge adapter path
// ============================================================================
//
// handleWithAdapters implements the experimental adapter dispatch path:
//
//   OpenAI ResponsesRequest
//     → ClientAdapter.ToCoreRequest()       → format.CoreRequest
//     → ProviderAdapter.FromCoreRequest()   → anthropic.MessageRequest (with cache injection)
//     → upstream provider.CreateMessage()   → anthropic.MessageResponse
//     → ProviderAdapter.ToCoreResponse()    → format.CoreResponse
//     → ClientAdapter.FromCoreResponse()    → openai.Response
//
// Streaming path:
//   OpenAI ResponsesRequest (stream=true)
//     → ClientAdapter.ToCoreRequest()       → format.CoreRequest
//     → ProviderAdapter.FromCoreRequest()   → anthropic.MessageRequest (with cache injection)
//     → upstream provider.StreamMessage()   → anthropic.Stream
//     → ProviderStreamAdapter.ToCoreStream()→ <-chan format.CoreStreamEvent
//     → ClientStreamAdapter.FromCoreStream()→ <-chan openai.StreamEvent
//     → write SSE events to ResponseWriter

// handleWithAdapters dispatches a request through the adapter path.
// Falls back to error when the required adapter is not found in the registry.
func (s *Server) handleWithAdapters(
	w http.ResponseWriter,
	r *http.Request,
	openAIReq openai.ResponsesRequest,
	route *provider.ResolvedRoute,
) {
	ctx := r.Context()
	log := slog.Default().With("model", openAIReq.Model, "path", "adapter")

	// Get or create session for this request.
	requestStart := time.Now()
	sess := s.sessionForRequest(r)
	_ = sess

	// Initialize trace record.
	bodyBytes, _ := json.Marshal(openAIReq)
	record := mbtrace.Record{
		HTTPRequest:   mbtrace.NewHTTPRequest(r),
		OpenAIRequest: mbtrace.RawJSONOrString(bodyBytes),
		Model:         openAIReq.Model,
	}
	defer func() {
		s.writeTrace(record)
	}()

	// ------------------------------------------------------------------
	// 1. Resolve inbound client adapter (always openai-response).
	// ------------------------------------------------------------------
	client, ok := s.adapterRegistry.GetClient(config.ProtocolOpenAIResponse)
	if !ok {
		log.Warn("adapter path: no client adapter for openai-response")
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		}
		record.Error = traceError("client_adapter", fmt.Errorf("no client adapter for openai-response"))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// ------------------------------------------------------------------
	// 2. Convert inbound OpenAI request → CoreRequest.
	// ------------------------------------------------------------------
	coreReq, err := client.ToCoreRequest(ctx, &openAIReq)
	if err != nil {
		log.Error("adapter path: ToCoreRequest failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("request conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		}
		record.Error = traceError("to_core_request", err)
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// ------------------------------------------------------------------
	// 3. Pick upstream provider candidate, resolve ProviderAdapter.
	// ------------------------------------------------------------------
	preferred, ok := route.Preferred()
	if !ok {
		log.Warn("adapter path: no provider candidate")
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		}
		record.Error = traceError("no_candidate", fmt.Errorf("no provider candidate"))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// Only handle anthropic protocol in this phase.
	if preferred.Protocol != config.ProtocolAnthropic {
		log.Warn("adapter path: unsupported protocol", "protocol", preferred.Protocol)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		}
		record.Error = traceError("unsupported_protocol", fmt.Errorf("unsupported protocol %q", preferred.Protocol))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	providerAdapter, ok := s.adapterRegistry.GetProvider(preferred.Protocol)
	if !ok {
		log.Warn("adapter path: no provider adapter for protocol", "protocol", preferred.Protocol)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		}
		record.Error = traceError("provider_adapter", fmt.Errorf("no provider adapter for %q", preferred.Protocol))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// ------------------------------------------------------------------
	// 4. Convert CoreRequest → upstream request (anthropic.MessageRequest).
	//    Cache planning/injection happens inside FromCoreRequest.
	// ------------------------------------------------------------------
	upstreamAny, err := providerAdapter.FromCoreRequest(ctx, coreReq)
	if err != nil {
		log.Error("adapter path: FromCoreRequest failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("upstream conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		}
		record.Error = traceError("from_core_request", err)
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}
	upstreamReq, ok := upstreamAny.(*anthropic.MessageRequest)
	if !ok {
		log.Error("adapter path: unexpected upstream type", "type", fmt.Sprintf("%T", upstreamAny))
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "unexpected upstream request type",
				Type:    "server_error",
				Code:    "internal_error",
			},
		}
		record.Error = traceError("upstream_type", fmt.Errorf("unexpected upstream type %T", upstreamAny))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// Inject web_search tool if enabled for this model.
	if s.providerMgr.ResolvedWebSearch(preferred.ProviderKey) == "enabled" {
		injectAnthropicWebSearch(upstreamReq)
	}

	// ------------------------------------------------------------------
	// 4b. If streaming, use streaming path.
	// ------------------------------------------------------------------
	if openAIReq.Stream {
		s.handleAdapterStream(w, r, ctx, openAIReq, coreReq, upstreamReq, preferred)
		return
	}

	// ------------------------------------------------------------------
	// 5. Call upstream provider (non-streaming).
	// ------------------------------------------------------------------
	effectiveProvider := s.resolveProvider(openAIReq.Model, route)
	if effectiveProvider == nil {
		log.Error("adapter path: no upstream provider resolved")
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("no upstream provider for model %q", openAIReq.Model),
				Type:    "server_error",
				Code:    "provider_error",
			},
		}
		record.Error = traceError("resolve_provider", fmt.Errorf("no upstream provider for %q", openAIReq.Model))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusBadGateway, payload)
		return
	}

	upstreamResp, err := effectiveProvider.CreateMessage(ctx, *upstreamReq)
	if err != nil {
		log.Error("adapter path: CreateMessage failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("upstream error: %v", err),
				Type:    "server_error",
				Code:    "provider_error",
			},
		}
		record.Error = traceError("create_message", err)
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusBadGateway, payload)
		return
	}

	// ------------------------------------------------------------------
	// 6. Convert upstream response → CoreResponse.
	// ------------------------------------------------------------------
	coreResp, err := providerAdapter.ToCoreResponse(ctx, &upstreamResp)
	if err != nil {
		log.Error("adapter path: ToCoreResponse failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("response conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		}
		record.Error = traceError("to_core_response", err)
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// ------------------------------------------------------------------
	// 7. Convert CoreResponse → outbound OpenAI Response.
	// ------------------------------------------------------------------
	outAny, err := client.FromCoreResponse(ctx, coreResp)
	if err != nil {
		log.Error("adapter path: FromCoreResponse failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("output conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		}
		record.Error = traceError("from_core_response", err)
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}
	out, ok := outAny.(*openai.Response)
	if !ok {
		log.Error("adapter path: unexpected output type", "type", fmt.Sprintf("%T", outAny))
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "unexpected output response type",
				Type:    "server_error",
				Code:    "internal_error",
			},
		}
		record.Error = traceError("output_type", fmt.Errorf("unexpected output type %T", outAny))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// ------------------------------------------------------------------
	// 8. Write the response.
	// ------------------------------------------------------------------
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(out)

	// Record trace with upstream details and final output.
	record.AnthropicRequest = upstreamReq
	record.AnthropicResponse = upstreamResp
	record.OpenAIResponse = out

	// Record completion via plugin hooks (placeholder).
	if s.pluginRegistry != nil {
		usage := zeroUsage(string(config.ProtocolAnthropic), "anthropic_response")
		if coreResp.Usage.InputTokens > 0 || coreResp.Usage.OutputTokens > 0 {
			usage = usageFromAnthropic(string(config.ProtocolAnthropic), "core_response", anthropic.Usage{
				InputTokens:              coreResp.Usage.InputTokens,
				OutputTokens:             coreResp.Usage.OutputTokens,
				CacheCreationInputTokens: 0,
				CacheReadInputTokens:     coreResp.Usage.CachedInputTokens,
			}, false)
		}
		s.onRequestCompleted(openAIReq.Model, openAIReq.Model, requestStart, usage, 0, "success", "")

		// Record usage statistics.
		if s.stats != nil {
			s.stats.Record(openAIReq.Model, preferred.UpstreamModel, stats.Usage{
				InputTokens:              coreResp.Usage.InputTokens,
				OutputTokens:             coreResp.Usage.OutputTokens,
				CacheReadInputTokens:     coreResp.Usage.CachedInputTokens,
				CacheCreationInputTokens: 0,
			})
		}
	}
}

// handleAdapterStream handles the streaming path through adapter dispatch.
func (s *Server) handleAdapterStream(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	openAIReq openai.ResponsesRequest,
	coreReq *format.CoreRequest,
	upstreamReq *anthropic.MessageRequest,
	candidate provider.ProviderCandidate,
) {
	log := slog.Default().With("model", openAIReq.Model, "path", "adapter_stream")

	// Track when the request started for latency measurement.
	requestStart := time.Now()

	// Get or create session for this request.
	sess := s.sessionForRequest(r)
	_ = sess

	// Initialize trace record.
	bodyBytes, _ := json.Marshal(openAIReq)
	streamRecord := mbtrace.Record{
		HTTPRequest:      mbtrace.NewHTTPRequest(r),
		OpenAIRequest:    mbtrace.RawJSONOrString(bodyBytes),
		AnthropicRequest: upstreamReq,
		Model:            openAIReq.Model,
	}
	defer func() {
		s.writeTrace(streamRecord)
	}()

	// Resolve provider for this candidate.
	effectiveProvider := s.resolveProvider(openAIReq.Model, &provider.ResolvedRoute{
		Candidates: []provider.ProviderCandidate{candidate},
	})
	if effectiveProvider == nil {
		log.Error("adapter stream: no upstream provider resolved")
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("no upstream provider for model %q", openAIReq.Model),
				Type:    "server_error",
				Code:    "provider_error",
			},
		}
		streamRecord.Error = traceError("stream_resolve_provider", fmt.Errorf("no upstream provider for %q", openAIReq.Model))
		streamRecord.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusBadGateway, payload)
		return
	}

	// Call upstream streaming API.
	stream, err := effectiveProvider.StreamMessage(ctx, *upstreamReq)
	if err != nil {
		log.Error("adapter stream: StreamMessage failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("upstream stream error: %v", err),
				Type:    "server_error",
				Code:    "provider_error",
			},
		}
		streamRecord.Error = traceError("stream_message", err)
		streamRecord.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusBadGateway, payload)
		return
	}
	defer stream.Close()

	// Get provider stream adapter.
	providerStream, ok := s.adapterRegistry.GetProviderStream(config.ProtocolAnthropic)
	if !ok {
		log.Warn("adapter stream: no provider stream adapter")
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter stream fallback not available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		}
		streamRecord.Error = traceError("stream_provider_adapter", fmt.Errorf("no provider stream adapter"))
		streamRecord.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// Convert upstream stream → CoreStreamEvent channel.
	coreEvents, err := providerStream.ToCoreStream(ctx, stream)
	if err != nil {
		log.Error("adapter stream: ToCoreStream failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("stream conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		}
		streamRecord.Error = traceError("stream_to_core", err)
		streamRecord.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// Get client stream adapter.
	clientStream, ok := s.adapterRegistry.GetClientStream(config.ProtocolOpenAIResponse)
	if !ok {
		log.Warn("adapter stream: no client stream adapter")
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter stream fallback not available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		}
		streamRecord.Error = traceError("stream_client_adapter", fmt.Errorf("no client stream adapter"))
		streamRecord.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// Convert CoreStreamEvent channel → OpenAI stream event channel.
	streamChanAny, err := clientStream.FromCoreStream(ctx, coreReq, coreEvents)
	if err != nil {
		log.Error("adapter stream: FromCoreStream failed", "error", err)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("client stream conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		}
		streamRecord.Error = traceError("stream_from_core", err)
		streamRecord.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	streamChan, ok := streamChanAny.(<-chan openai.StreamEvent)
	if !ok {
		log.Error("adapter stream: unexpected stream channel type", "type", fmt.Sprintf("%T", streamChanAny))
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "unexpected stream channel type",
				Type:    "server_error",
				Code:    "internal_error",
			},
		}
		streamRecord.Error = traceError("stream_channel_type", fmt.Errorf("unexpected stream channel type %T", streamChanAny))
		streamRecord.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}

	// Write SSE events.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	// Track usage from the final response.completed event.
	var finalUsage openai.Usage
	var finalResp *openai.Response
	for ev := range streamChan {
		if ev.Event == "response.completed" {
			if lf, ok := ev.Data.(openai.ResponseLifecycleEvent); ok {
				finalUsage = lf.Response.Usage
				lfResp := lf.Response
				finalResp = &lfResp
			}
		}
		writeSSE(w, ev)
	}

	// Record usage statistics after stream completes.
	if s.stats != nil && (finalUsage.InputTokens > 0 || finalUsage.OutputTokens > 0) {
		s.stats.Record(openAIReq.Model, candidate.UpstreamModel, stats.Usage{
			InputTokens:              finalUsage.InputTokens,
			OutputTokens:             finalUsage.OutputTokens,
			CacheCreationInputTokens: finalUsage.InputTokensDetails.CachedTokens,
			CacheReadInputTokens:     0,
		})
	}

	// Update trace record with the final response data.
	if finalResp != nil {
		streamRecord.OpenAIResponse = finalResp
	}

	// Notify plugin hooks for metrics tracking.
	if s.pluginRegistry != nil && finalResp != nil {
		usage := zeroUsage(string(config.ProtocolAnthropic), "anthropic_stream")
		if finalUsage.InputTokens > 0 || finalUsage.OutputTokens > 0 {
			usage = usageFromAnthropic(string(config.ProtocolAnthropic), "core_stream", anthropic.Usage{
				InputTokens:              finalUsage.InputTokens,
				OutputTokens:             finalUsage.OutputTokens,
				CacheCreationInputTokens: 0,
				CacheReadInputTokens:     finalUsage.InputTokensDetails.CachedTokens,
			}, false)
		}
		s.onRequestCompleted(openAIReq.Model, candidate.UpstreamModel, requestStart, usage, 0, "success", "")
	}
}

// ============================================================================
// adapterCacheManager — implements anthropic.CacheManager
// ============================================================================

// adapterCacheManager wraps the cache package to provide cache planning and
// registry updates for the AnthropicProviderAdapter in the adapter dispatch path.
type adapterCacheManager struct {
	cacheCfg cache.PlanCacheConfig
	registry *cache.MemoryRegistry
}

// NewAdapterCacheManager creates a new CacheManager for the adapter path.
func NewAdapterCacheManager(cfg config.CacheConfig, registry *cache.MemoryRegistry) anthropic.CacheManager {
	return &adapterCacheManager{
		cacheCfg: cache.PlanCacheConfig{
			Mode:                     cfg.Mode,
			TTL:                      cfg.TTL,
			PromptCaching:            cfg.PromptCaching,
			AutomaticPromptCache:     cfg.AutomaticPromptCache,
			ExplicitCacheBreakpoints: cfg.ExplicitCacheBreakpoints,
			AllowRetentionDowngrade:  cfg.AllowRetentionDowngrade,
			MaxBreakpoints:           cfg.MaxBreakpoints,
			MinCacheTokens:           cfg.MinCacheTokens,
			ExpectedReuse:            cfg.ExpectedReuse,
			MinimumValueScore:        cfg.MinimumValueScore,
			MinBreakpointTokens:      cfg.MinBreakpointTokens,
		},
		registry: registry,
	}
}

// PlanAndInject implements anthropic.CacheManager.
// It extracts cache metadata from coreReq.Extensions["cache"], builds a cache plan,
// injects cache_control into the anthropic request, and returns the cache key + TTL.
func (m *adapterCacheManager) PlanAndInject(ctx context.Context, req *anthropic.MessageRequest, coreReq *format.CoreRequest) (key, ttl string) {
	// Extract cache metadata from CoreRequest.Extensions["cache"].
	promptCacheKey := ""
	promptCacheRetention := ""
	if ext, ok := coreReq.Extensions["cache"]; ok {
		if cacheMeta, ok := ext.(map[string]any); ok {
			if k, ok := cacheMeta["prompt_cache_key"].(string); ok {
				promptCacheKey = k
			}
			if r, ok := cacheMeta["prompt_cache_retention"].(string); ok {
				promptCacheRetention = r
			}
		}
	}

	// Build a minimal openai.ResponsesRequest subset for cache planning.
	openaiReq := openai.ResponsesRequest{
		PromptCacheKey:       promptCacheKey,
		PromptCacheRetention: promptCacheRetention,
	}

	plan, err := cache.PlanCache(m.cacheCfg, m.registry, openaiReq, *req)
	if err != nil {
		slog.Warn("adapter cache planning failed", "error", err)
		return "", ""
	}

	cache.InjectCacheControl(req, plan)
	return plan.PrefixKey, plan.TTL
}

// UpdateRegistry implements anthropic.CacheManager.
// It updates the in-memory cache registry from upstream usage signals.
func (m *adapterCacheManager) UpdateRegistry(ctx context.Context, key, ttl string, usage anthropic.Usage) {
	if key == "" {
		return
	}
	signals := cache.UsageSignals{
		InputTokens:              usage.InputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
	cache.UpdateRegistryFromUsage(m.registry, cache.CacheCreationPlan{
		PrefixKey: key,
		TTL:       ttl,
	}, signals, usage.InputTokens)
}

// injectAnthropicWebSearch adds the Anthropic web_search_20250305 server tool
// to an anthropic.MessageRequest if not already present.
func injectAnthropicWebSearch(req *anthropic.MessageRequest) {
	for i, t := range req.Tools {
		if t.Name == "web_search" {
			// Already present — ensure Type is set correctly for Anthropic API.
			if t.Type != "web_search_20250305" && t.Type != "web_search_20260209" {
				req.Tools[i].Type = "web_search_20250305"
			}
			return
		}
	}
	maxUses := 8
	if req.Tools == nil {
		req.Tools = make([]anthropic.Tool, 0, 1)
	}
	req.Tools = append(req.Tools, anthropic.Tool{
		Name:    "web_search",
		Type:    "web_search_20250305",
		MaxUses: maxUses,
	})
}
