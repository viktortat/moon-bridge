package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"moonbridge/internal/foundation/config"
	openai "moonbridge/internal/protocol/openai"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/cache"
	"moonbridge/internal/protocol/format"
	"moonbridge/internal/service/provider"
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

	// ------------------------------------------------------------------
	// 1. Resolve inbound client adapter (always openai-response).
	// ------------------------------------------------------------------
	client, ok := s.adapterRegistry.GetClient(config.ProtocolOpenAIResponse)
	if !ok {
		log.Warn("adapter path: no client adapter for openai-response")
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		})
		return
	}

	// ------------------------------------------------------------------
	// 2. Convert inbound OpenAI request → CoreRequest.
	// ------------------------------------------------------------------
	coreReq, err := client.ToCoreRequest(ctx, &openAIReq)
	if err != nil {
		log.Error("adapter path: ToCoreRequest failed", "error", err)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("request conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		})
		return
	}

	// ------------------------------------------------------------------
	// 3. Pick upstream provider candidate, resolve ProviderAdapter.
	// ------------------------------------------------------------------
	preferred, ok := route.Preferred()
	if !ok {
		log.Warn("adapter path: no provider candidate")
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		})
		return
	}

	// Only handle anthropic protocol in this phase.
	if preferred.Protocol != config.ProtocolAnthropic {
		log.Warn("adapter path: unsupported protocol", "protocol", preferred.Protocol)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		})
		return
	}

	providerAdapter, ok := s.adapterRegistry.GetProvider(preferred.Protocol)
	if !ok {
		log.Warn("adapter path: no provider adapter for protocol", "protocol", preferred.Protocol)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter path precondition failed: no fallback available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		})
		return
	}

	// ------------------------------------------------------------------
	// 4. Convert CoreRequest → upstream request (anthropic.MessageRequest).
	//    Cache planning/injection happens inside FromCoreRequest.
	// ------------------------------------------------------------------
	upstreamAny, err := providerAdapter.FromCoreRequest(ctx, coreReq)
	if err != nil {
		log.Error("adapter path: FromCoreRequest failed", "error", err)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("upstream conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		})
		return
	}
	upstreamReq, ok := upstreamAny.(*anthropic.MessageRequest)
	if !ok {
		log.Error("adapter path: unexpected upstream type", "type", fmt.Sprintf("%T", upstreamAny))
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "unexpected upstream request type",
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	// Inject web_search tool if enabled for this model.
	if s.providerMgr.ResolvedWebSearchForModel(openAIReq.Model) == "enabled" {
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
		writeOpenAIError(w, http.StatusBadGateway, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("no upstream provider for model %q", openAIReq.Model),
				Type:    "server_error",
				Code:    "provider_error",
			},
		})
		return
	}

	upstreamResp, err := effectiveProvider.CreateMessage(ctx, *upstreamReq)
	if err != nil {
		log.Error("adapter path: CreateMessage failed", "error", err)
		writeOpenAIError(w, http.StatusBadGateway, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("upstream error: %v", err),
				Type:    "server_error",
				Code:    "provider_error",
			},
		})
		return
	}

	// ------------------------------------------------------------------
	// 6. Convert upstream response → CoreResponse.
	// ------------------------------------------------------------------
	coreResp, err := providerAdapter.ToCoreResponse(ctx, &upstreamResp)
	if err != nil {
		log.Error("adapter path: ToCoreResponse failed", "error", err)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("response conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		})
		return
	}

	// ------------------------------------------------------------------
	// 7. Convert CoreResponse → outbound OpenAI Response.
	// ------------------------------------------------------------------
	outAny, err := client.FromCoreResponse(ctx, coreResp)
	if err != nil {
		log.Error("adapter path: FromCoreResponse failed", "error", err)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("output conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		})
		return
	}
	out, ok := outAny.(*openai.Response)
	if !ok {
		log.Error("adapter path: unexpected output type", "type", fmt.Sprintf("%T", outAny))
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "unexpected output response type",
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	// ------------------------------------------------------------------
	// 8. Write the response.
	// ------------------------------------------------------------------
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(out)
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

	// Resolve provider for this candidate.
	effectiveProvider := s.resolveProvider(openAIReq.Model, &provider.ResolvedRoute{
		Candidates: []provider.ProviderCandidate{candidate},
	})
	if effectiveProvider == nil {
		log.Error("adapter stream: no upstream provider resolved")
		writeOpenAIError(w, http.StatusBadGateway, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("no upstream provider for model %q", openAIReq.Model),
				Type:    "server_error",
				Code:    "provider_error",
			},
		})
		return
	}

	// Call upstream streaming API.
	stream, err := effectiveProvider.StreamMessage(ctx, *upstreamReq)
	if err != nil {
		log.Error("adapter stream: StreamMessage failed", "error", err)
		writeOpenAIError(w, http.StatusBadGateway, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("upstream stream error: %v", err),
				Type:    "server_error",
				Code:    "provider_error",
			},
		})
		return
	}
	defer stream.Close()

	// Get provider stream adapter.
	providerStream, ok := s.adapterRegistry.GetProviderStream(config.ProtocolAnthropic)
	if !ok {
		log.Warn("adapter stream: no provider stream adapter")
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter stream fallback not available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		})
		return
	}

	// Convert upstream stream → CoreStreamEvent channel.
	coreEvents, err := providerStream.ToCoreStream(ctx, stream)
	if err != nil {
		log.Error("adapter stream: ToCoreStream failed", "error", err)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("stream conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		})
		return
	}

	// Get client stream adapter.
	clientStream, ok := s.adapterRegistry.GetClientStream(config.ProtocolOpenAIResponse)
	if !ok {
		log.Warn("adapter stream: no client stream adapter")
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "adapter stream fallback not available",
				Type:    "server_error",
				Code:    "adapter_fallback",
			},
		})
		return
	}

	// Convert CoreStreamEvent channel → OpenAI stream event channel.
	streamChanAny, err := clientStream.FromCoreStream(ctx, coreReq, coreEvents)
	if err != nil {
		log.Error("adapter stream: FromCoreStream failed", "error", err)
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("client stream conversion failed: %v", err),
				Type:    "server_error",
				Code:    "conversion_error",
			},
		})
		return
	}

	streamChan, ok := streamChanAny.(<-chan openai.StreamEvent)
	if !ok {
		log.Error("adapter stream: unexpected stream channel type", "type", fmt.Sprintf("%T", streamChanAny))
		writeOpenAIError(w, http.StatusInternalServerError, openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "unexpected stream channel type",
				Type:    "server_error",
				Code:    "internal_error",
			},
		})
		return
	}

	// Write SSE events.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	for ev := range streamChan {
		writeSSE(w, ev)
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
	for _, t := range req.Tools {
		if t.Name == "web_search" {
			return // already present
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
