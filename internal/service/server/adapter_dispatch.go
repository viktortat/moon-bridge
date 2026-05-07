package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	deepseekv4 "moonbridge/internal/extension/deepseek_v4"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/foundation/config"
	"moonbridge/internal/foundation/session"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/format"
	openai "moonbridge/internal/protocol/openai"
	"moonbridge/internal/protocol/chat"
	"moonbridge/internal/protocol/google"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/stats"
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

	// Defense-in-depth: ensure model is non-empty.
	if openAIReq.Model == "" {
		log.Warn("adapter path: empty model")
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: "model is required",
				Type:    "invalid_request_error",
				Code:    "missing_model",
			},
		}
		writeOpenAIError(w, http.StatusBadRequest, payload)
		return
	}

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
	// Protocol-specific type assertion and upstream call.
	var coreResp *format.CoreResponse
	switch preferred.Protocol {
	case config.ProtocolAnthropic:
		upstreamReq, ok := upstreamAny.(*anthropic.MessageRequest)
		if !ok {
			log.Error("adapter path: unexpected anthropic upstream type", "type", fmt.Sprintf("%T", upstreamAny))
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "unexpected anthropic upstream request type",
					Type:    "server_error",
					Code:    "internal_error",
				},
			}
			record.Error = traceError("upstream_type", fmt.Errorf("unexpected anthropic type %T", upstreamAny))
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

		// Inject web_search tool if enabled for this model.
		if s.providerMgr.ResolvedWebSearch(preferred.ProviderKey) == "enabled" {
			injectAnthropicWebSearch(upstreamReq)
		}

		// Prepend cached reasoning blocks for DeepSeek thinking chain replay.
		if s.pluginRegistry != nil && sess != nil {
			prependCachedThinking(upstreamReq, sess)
		}

		// If streaming, use streaming path.
		if openAIReq.Stream {
			s.handleAdapterStream(w, r, ctx, openAIReq, coreReq, upstreamReq, preferred)
			record.OpenAIRequest = nil
			return
		}

		// Non-streaming upstream call.
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

		// Anthropic response → CoreResponse.
		coreResp, err = providerAdapter.ToCoreResponse(ctx, &upstreamResp)

		// Remember response content for plugin state tracking (DeepSeek thinking replay).
		if s.pluginRegistry != nil && len(upstreamResp.Content) > 0 {
			s.pluginRegistry.RememberContent(
				&plugin.RequestContext{
					ModelAlias:  openAIReq.Model,
					SessionData: sess.ExtensionData,
				},
				upstreamResp.Content,
			)
		}

	case config.ProtocolOpenAIChat:
		chatReq, ok := upstreamAny.(*chat.ChatRequest)
		if !ok {
			log.Error("adapter path: unexpected chat upstream type", "type", fmt.Sprintf("%T", upstreamAny))
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "unexpected chat upstream request type",
					Type:    "server_error",
					Code:    "internal_error",
				},
			}
			record.Error = traceError("upstream_type", fmt.Errorf("unexpected chat type %T", upstreamAny))
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

		if openAIReq.Stream {
			s.handleAdapterStream(w, r, ctx, openAIReq, coreReq, chatReq, preferred)
			record.OpenAIRequest = nil
			return
		}

		chatClient, ok := s.chatClients[preferred.ProviderKey]
		if !ok {
			log.Error("adapter path: no chat client for provider", "provider", preferred.ProviderKey)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("no chat client for provider %q", preferred.ProviderKey),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			record.Error = traceError("chat_client", fmt.Errorf("no chat client for %q", preferred.ProviderKey))
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		chatResp, err := chatClient.CreateChat(ctx, chatReq)
		if err != nil {
			log.Error("adapter path: Chat API call failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("chat upstream error: %v", err),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			record.Error = traceError("chat_api", err)
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		coreResp, err = providerAdapter.ToCoreResponse(ctx, chatResp)
		if err != nil {
			log.Error("adapter path: Chat ToCoreResponse failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("chat response conversion failed: %v", err),
					Type:    "server_error",
					Code:    "conversion_error",
				},
			}
			record.Error = traceError("to_core_response", err)
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

	case config.ProtocolGoogleGenAI:
		googleReq, ok := upstreamAny.(*google.GenerateContentRequest)
		if !ok {
			log.Error("adapter path: unexpected google upstream type", "type", fmt.Sprintf("%T", upstreamAny))
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "unexpected google upstream request type",
					Type:    "server_error",
					Code:    "internal_error",
				},
			}
			record.Error = traceError("upstream_type", fmt.Errorf("unexpected google type %T", upstreamAny))
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

		if openAIReq.Stream {
			s.handleAdapterStream(w, r, ctx, openAIReq, coreReq, googleReq, preferred)
			record.OpenAIRequest = nil
			return
		}

		googleClient, ok := s.googleClients[preferred.ProviderKey]
		if !ok {
			log.Error("adapter path: no google client for provider", "provider", preferred.ProviderKey)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("no google client for provider %q", preferred.ProviderKey),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			record.Error = traceError("google_client", fmt.Errorf("no google client for %q", preferred.ProviderKey))
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		googleResp, err := googleClient.GenerateContent(ctx, preferred.UpstreamModel, googleReq)
		if err != nil {
			log.Error("adapter path: Google API call failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("google upstream error: %v", err),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			record.Error = traceError("google_api", err)
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		coreResp, err = providerAdapter.ToCoreResponse(ctx, googleResp)
		if err != nil {
			log.Error("adapter path: Google ToCoreResponse failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("google response conversion failed: %v", err),
					Type:    "server_error",
					Code:    "conversion_error",
				},
			}
			record.Error = traceError("to_core_response", err)
			record.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

	default:
		log.Error("adapter path: unsupported protocol", "protocol", preferred.Protocol)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("unsupported protocol %q", preferred.Protocol),
				Type:    "server_error",
				Code:    "adapter_not_configured",
			},
		}
		record.Error = traceError("unsupported_protocol", fmt.Errorf("unsupported protocol %q", preferred.Protocol))
		record.OpenAIResponse = payload
		writeOpenAIError(w, http.StatusInternalServerError, payload)
		return
	}
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
			}, true) // input tokens now include cache (normalized at adapter level)
		}
		s.onRequestCompleted(openAIReq.Model, preferred.UpstreamModel, preferred.ProviderKey, requestStart, usage, 0, "success", "")

		// Record usage statistics.
		if s.stats != nil {
			s.stats.Record(openAIReq.Model, preferred.UpstreamModel, stats.Usage{
				InputTokens:              coreResp.Usage.InputTokens,
				OutputTokens:             coreResp.Usage.OutputTokens,
				CacheReadInputTokens:     coreResp.Usage.CachedInputTokens,
				CacheCreationInputTokens: 0,
			})
		}

		// Log detailed metrics for non-streaming request.
		inputTotal := coreResp.Usage.InputTokens
		cachedInput := coreResp.Usage.CachedInputTokens
		freshInput := inputTotal - cachedInput
		if freshInput < 0 {
			freshInput = 0
		}
		outputTokens := coreResp.Usage.OutputTokens
		var cacheHitRate float64
		if inputTotal > 0 {
			cacheHitRate = float64(cachedInput) / float64(inputTotal) * 100
		}
		reqDuration := time.Since(requestStart)
		billingUsage := stats.BillingUsage{
			FreshInputTokens:         freshInput,
			OutputTokens:             outputTokens,
			CacheCreationInputTokens: 0,
			CacheReadInputTokens:     cachedInput,
		}
		reqCost := computeCostWithProviderPricing(s.providerMgr, s.stats, openAIReq.Model, preferred.UpstreamModel, preferred.ProviderKey, billingUsage)
		log.Info("请求完成",
			"request_model", openAIReq.Model,
			"actual_model", preferred.UpstreamModel,
			"provider", preferred.ProviderKey,
			"input_total", inputTotal,
			"input_fresh", freshInput,
			"input_cache_read", cachedInput,
			"input_cache_write", 0,
			"output_tokens", outputTokens,
			"cache_hit_rate", fmt.Sprintf("%.1f%%", cacheHitRate),
			"request_cost", reqCost,
			"duration", reqDuration,
		)
	}
}

// handleAdapterStream handles the streaming path through adapter dispatch.
func (s *Server) handleAdapterStream(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	openAIReq openai.ResponsesRequest,
	coreReq *format.CoreRequest,
	upstreamReq any,
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
		HTTPRequest:   mbtrace.NewHTTPRequest(r),
		OpenAIRequest: mbtrace.RawJSONOrString(bodyBytes),
		Model:         openAIReq.Model,
	}
	defer func() {
		s.writeTrace(streamRecord)
	}()

	// Protocol-specific upstream streaming: get stream + convert to CoreStreamEvent.
	var coreEvents <-chan format.CoreStreamEvent
	var providerStream format.ProviderStreamAdapter

	switch candidate.Protocol {
	case config.ProtocolAnthropic:
		anthReq, ok := upstreamReq.(*anthropic.MessageRequest)
		if !ok {
			log.Error("adapter stream: unexpected anthropic type")
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "unexpected anthropic upstream type",
					Type:    "server_error",
					Code:    "internal_error",
				},
			}
			streamRecord.Error = traceError("stream_type", fmt.Errorf("unexpected anthropic type"))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}
		streamRecord.AnthropicRequest = anthReq

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

		stream, err := effectiveProvider.StreamMessage(ctx, *anthReq)
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

		providerStream, ok = s.adapterRegistry.GetProviderStream(config.ProtocolAnthropic)
		if !ok {
			log.Warn("adapter stream: no anthropic provider stream adapter")
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "adapter stream fallback not available",
					Type:    "server_error",
					Code:    "adapter_fallback",
				},
			}
			streamRecord.Error = traceError("stream_provider_adapter", fmt.Errorf("no anthropic provider stream adapter"))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}
		coreEvents, err = providerStream.ToCoreStream(ctx, stream)
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

	case config.ProtocolOpenAIChat:
		chatReq, ok := upstreamReq.(*chat.ChatRequest)
		if !ok {
			log.Error("adapter stream: unexpected chat type")
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "unexpected chat upstream type",
					Type:    "server_error",
					Code:    "internal_error",
				},
			}
			streamRecord.Error = traceError("stream_type", fmt.Errorf("unexpected chat type"))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

		chatClient, ok := s.chatClients[candidate.ProviderKey]
		if !ok {
			log.Error("adapter stream: no chat client", "provider", candidate.ProviderKey)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("no chat client for provider %q", candidate.ProviderKey),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			streamRecord.Error = traceError("stream_chat_client", fmt.Errorf("no chat client for %q", candidate.ProviderKey))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		chatStream, err := chatClient.StreamChat(ctx, chatReq)
		if err != nil {
			log.Error("adapter stream: StreamChat failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("chat stream error: %v", err),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			streamRecord.Error = traceError("stream_chat", err)
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		providerStream, ok = s.adapterRegistry.GetProviderStream(config.ProtocolOpenAIChat)
		if !ok {
			log.Warn("adapter stream: no chat provider stream adapter")
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "chat stream adapter not available",
					Type:    "server_error",
					Code:    "adapter_fallback",
				},
			}
			streamRecord.Error = traceError("stream_chat_adapter", fmt.Errorf("no chat provider stream adapter"))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}
		coreEvents, err = providerStream.ToCoreStream(ctx, chatStream)
		if err != nil {
			log.Error("adapter stream: Chat ToCoreStream failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("chat stream conversion failed: %v", err),
					Type:    "server_error",
					Code:    "conversion_error",
				},
			}
			streamRecord.Error = traceError("stream_chat_tocore", err)
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

	case config.ProtocolGoogleGenAI:
		googleReq, ok := upstreamReq.(*google.GenerateContentRequest)
		if !ok {
			log.Error("adapter stream: unexpected google type")
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "unexpected google upstream type",
					Type:    "server_error",
					Code:    "internal_error",
				},
			}
			streamRecord.Error = traceError("stream_type", fmt.Errorf("unexpected google type"))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

		googleClient, ok := s.googleClients[candidate.ProviderKey]
		if !ok {
			log.Error("adapter stream: no google client", "provider", candidate.ProviderKey)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("no google client for provider %q", candidate.ProviderKey),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			streamRecord.Error = traceError("stream_google_client", fmt.Errorf("no google client for %q", candidate.ProviderKey))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		googleStream, err := googleClient.StreamGenerateContent(ctx, candidate.UpstreamModel, googleReq)
		if err != nil {
			log.Error("adapter stream: StreamGenerateContent failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("google stream error: %v", err),
					Type:    "server_error",
					Code:    "provider_error",
				},
			}
			streamRecord.Error = traceError("stream_google", err)
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusBadGateway, payload)
			return
		}

		providerStream, ok = s.adapterRegistry.GetProviderStream(config.ProtocolGoogleGenAI)
		if !ok {
			log.Warn("adapter stream: no google provider stream adapter")
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: "google stream adapter not available",
					Type:    "server_error",
					Code:    "adapter_fallback",
				},
			}
			streamRecord.Error = traceError("stream_google_adapter", fmt.Errorf("no google provider stream adapter"))
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}
		coreEvents, err = providerStream.ToCoreStream(ctx, googleStream)
		if err != nil {
			log.Error("adapter stream: Google ToCoreStream failed", "error", err)
			payload := openai.ErrorResponse{
				Error: openai.ErrorObject{
					Message: fmt.Sprintf("google stream conversion failed: %v", err),
					Type:    "server_error",
					Code:    "conversion_error",
				},
			}
			streamRecord.Error = traceError("stream_google_tocore", err)
			streamRecord.OpenAIResponse = payload
			writeOpenAIError(w, http.StatusInternalServerError, payload)
			return
		}

	default:
		log.Error("adapter stream: unsupported protocol", "protocol", candidate.Protocol)
		payload := openai.ErrorResponse{
			Error: openai.ErrorObject{
				Message: fmt.Sprintf("unsupported stream protocol %q", candidate.Protocol),
				Type:    "server_error",
				Code:    "adapter_not_configured",
			},
		}
		streamRecord.Error = traceError("stream_unsupported_protocol", fmt.Errorf("unsupported protocol %q", candidate.Protocol))
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
		if err := writeSSE(w, ev); err != nil {
			log.Warn("adapter stream: SSE write failed, aborting stream", "error", err)
			break
		}
	}

	// Record usage statistics after stream completes.

	// Capture stream events for trace.
	if s.tracer != nil && s.tracer.Enabled() {
		// OpenAI stream events from client adapter
		if oaiClient, ok := s.adapterRegistry.GetClient(config.ProtocolOpenAIResponse); ok {
			if oaiAdapter, ok := oaiClient.(*openai.OpenAIAdapter); ok {
				if events := oaiAdapter.StreamBuffer(); len(events) > 0 {
					streamRecord.OpenAIStreamEvents = events
				}
			}
		}
		// Anthropic stream events from provider adapter
		if anthProvider, ok := s.adapterRegistry.GetProvider(config.ProtocolAnthropic); ok {
			if anthAdapter, ok := anthProvider.(*anthropic.AnthropicProviderAdapter); ok {
				if events := anthAdapter.StreamBuffer(); len(events) > 0 {
					streamRecord.AnthropicStreamEvents = events
				}

				// Remember reasoning content for DeepSeek thinking replay via StreamInterceptor.
				if anthProvider2, ok := s.adapterRegistry.GetProvider(config.ProtocolAnthropic); ok {
					if anthAdapter2, ok := anthProvider2.(*anthropic.AnthropicProviderAdapter); ok {
						if s.pluginRegistry != nil && sess != nil {
							events := anthAdapter2.StreamBuffer()
							if len(events) > 0 {
								states := s.pluginRegistry.NewStreamStates(openAIReq.Model)
								for _, ev := range events {
									pluginType := ""
									switch {
									case ev.Type == "content_block_start":
										pluginType = "block_start"
									case ev.Type == "content_block_delta":
										pluginType = "block_delta"
									case ev.Type == "content_block_stop":
										pluginType = "block_stop"
									}
									if pluginType == "" {
										continue
									}
									s.pluginRegistry.OnStreamEvent(openAIReq.Model, plugin.StreamEvent{
										Type:  pluginType,
										Index: ev.Index,
										Block: ev.ContentBlock,
										Delta: ev.Delta,
									}, states)
								}
								outputText := ""
								if finalResp != nil {
									outputText = finalResp.OutputText
								}
								s.pluginRegistry.OnStreamComplete(openAIReq.Model, states, outputText, sess.ExtensionData)
							}
						}
					}
				}
			}
		}
	}
	if s.stats != nil && (finalUsage.InputTokens > 0 || finalUsage.OutputTokens > 0) {
		s.stats.Record(openAIReq.Model, candidate.UpstreamModel, stats.Usage{
			InputTokens:              finalUsage.InputTokens,
			OutputTokens:             finalUsage.OutputTokens,
			CacheCreationInputTokens: 0,
			CacheReadInputTokens:     finalUsage.InputTokensDetails.CachedTokens,
		})
	}

	inputTotal := finalUsage.InputTokens
	cachedInput := finalUsage.InputTokensDetails.CachedTokens
	freshInput := inputTotal - cachedInput
	if freshInput < 0 {
		freshInput = 0
	}
	outputTokens := finalUsage.OutputTokens
	var cacheHitRate float64
	if inputTotal > 0 {
		cacheHitRate = float64(cachedInput) / float64(inputTotal) * 100
	}
	reqDuration := time.Since(requestStart)
	billingUsage := stats.BillingUsage{
		FreshInputTokens:         freshInput,
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: finalUsage.InputTokensDetails.CachedTokens,
		CacheReadInputTokens:     0,
	}
	reqCost := computeCostWithProviderPricing(s.providerMgr, s.stats, openAIReq.Model, candidate.UpstreamModel, candidate.ProviderKey, billingUsage)
	log.Info("流式请求完成",
		"model", openAIReq.Model,
		"actual_model", candidate.UpstreamModel,
		"provider", candidate.ProviderKey,
		"input_total", inputTotal,
		"input_fresh", freshInput,
		"input_cached_tokens", cachedInput,
		"output_tokens", outputTokens,
		"cache_hit_rate", fmt.Sprintf("%.1f%%", cacheHitRate),
		"request_cost", reqCost,
		"duration", reqDuration,
	)

	// Update trace record with the final response data.
	if finalResp != nil {
		streamRecord.OpenAIResponse = finalResp
	} else {
		streamRecord.OpenAIResponse = &openai.Response{Model: openAIReq.Model, Status: "completed"}
	}

	// Notify plugin hooks for metrics tracking.
	if s.pluginRegistry != nil {
		usage := zeroUsage(string(config.ProtocolAnthropic), "anthropic_stream")
		if finalUsage.InputTokens > 0 || finalUsage.OutputTokens > 0 {
			usage = usageFromAnthropic(string(config.ProtocolAnthropic), "core_stream", anthropic.Usage{
				InputTokens:              finalUsage.InputTokens,
				OutputTokens:             finalUsage.OutputTokens,
				CacheCreationInputTokens: 0,
				CacheReadInputTokens:     finalUsage.InputTokensDetails.CachedTokens,
			}, true) // input tokens now include cache (normalized at adapter level)
		}
		s.onRequestCompleted(openAIReq.Model, candidate.UpstreamModel, candidate.ProviderKey, requestStart, usage, 0, "success", "")
	}
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

// prependCachedThinking restores thinking blocks before assistant messages
// for DeepSeek thinking chain replay across conversation turns.
// It looks up cached thinking blocks from the session state and prepends them
// before tool_use and text assistant messages in the upstream request.
//
// Important: unlike PrependThinkingBlockForToolUse (which always targets the
// LAST message), this function targets the SPECIFIC assistant message that
// contains the tool_use, because in follow-up requests the last message
// is typically a user tool_result.
func prependCachedThinking(upstreamReq *anthropic.MessageRequest, sess *session.Session) {
	stateRaw, ok := sess.ExtensionData["deepseek_v4"]
	if !ok {
		return
	}
	state, ok := stateRaw.(*deepseekv4.State)
	if !ok {
		return
	}

	// For each assistant message, prepend cached thinking from the previous turn.
	for i := range upstreamReq.Messages {
		msg := &upstreamReq.Messages[i]
		if msg.Role != "assistant" || len(msg.Content) == 0 {
			continue
		}
		// Check if the message already has a thinking block.
		if hasThinkingBlock(msg.Content) {
			continue
		}
		// Try to prepend cached thinking by tool call ID (for tool_use messages).
		for _, block := range msg.Content {
			if block.Type == "tool_use" && block.ID != "" {
				if cached, ok := state.CachedForToolCall(block.ID); ok {
					// Prepend thinking block directly to this message, not to the last message.
					msg.Content = append([]anthropic.ContentBlock{normalizeThinkingBlock(cached)}, msg.Content...)
				}
				break
			}
		}
		// Fallback: try text-based caching (for text-only assistant messages).
		if !hasThinkingBlock(msg.Content) {
			prepended := state.PrependCachedForAssistantText(msg.Content)
			if len(prepended) > len(msg.Content) {
				msg.Content = prepended
			}
		}
	}
}

// normalizeThinkingBlock ensures a thinking block has the correct Type field.
func normalizeThinkingBlock(block anthropic.ContentBlock) anthropic.ContentBlock {
	return anthropic.ContentBlock{
		Type:      "thinking",
		Thinking:  block.Thinking,
		Signature: block.Signature,
	}
}

// hasThinkingBlock checks if anthropic message content contains a thinking block.
func hasThinkingBlock(content []anthropic.ContentBlock) bool {
	for _, block := range content {
		if block.Type == "thinking" {
			return true
		}
	}
	return false
}
