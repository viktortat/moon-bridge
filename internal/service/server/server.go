package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"log/slog"
	"moonbridge/internal/extension/codex"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/extension/visual"
	"moonbridge/internal/extension/websearchinjected"
	"moonbridge/internal/foundation/config"
	"moonbridge/internal/foundation/openai"
	"moonbridge/internal/foundation/session"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/bridge"
	"moonbridge/internal/protocol/cache"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/stats"
	mbtrace "moonbridge/internal/service/trace"

	"moonbridge/internal/service/api"
	"moonbridge/internal/service/runtime"
	"moonbridge/internal/service/store"
)

// Provider defines the upstream interface for creating messages.
type Provider interface {
	CreateMessage(context.Context, anthropic.MessageRequest) (anthropic.MessageResponse, error)
	StreamMessage(context.Context, anthropic.MessageRequest) (anthropic.Stream, error)
}

type Config struct {
	Bridge           *bridge.Bridge
	Provider         Provider
	ProviderMgr      *provider.ProviderManager // optional; used for multi-provider routing
	OpenAIHTTPClient *http.Client
	Tracer           *mbtrace.Tracer
	TraceErrors      io.Writer
	Stats            *stats.SessionStats
	PluginRegistry   *plugin.Registry
	AppConfig        config.Config // full app config for per-provider resolution
	Runtime  *runtime.Runtime  // optional; when set, Current().Config takes priority over AppConfig
	Store    store.ConfigStore  // optional; API management endpoints
}

type Server struct {
	bridge           *bridge.Bridge
	provider         Provider
	providerMgr      *provider.ProviderManager
	openAIHTTP       *http.Client
	tracer           *mbtrace.Tracer
	traceErrors      io.Writer
	stats            *stats.SessionStats
	pluginRegistry   *plugin.Registry
	mux              *http.ServeMux
	sessionsMu       sync.Mutex
	sessions         map[string]serverSession
	sessionPruneStop chan struct{}
	onceClose        sync.Once
	appConfig        config.Config
	runtime          *runtime.Runtime
	store            store.ConfigStore
}

type serverSession struct {
	sess     *session.Session
	lastUsed time.Time
}

const sessionTTL = 24 * time.Hour

func New(cfg Config) *Server {
	server := &Server{
		bridge:           cfg.Bridge,
		provider:         cfg.Provider,
		providerMgr:      cfg.ProviderMgr,
		openAIHTTP:       cfg.OpenAIHTTPClient,
		tracer:           cfg.Tracer,
		traceErrors:      cfg.TraceErrors,
		stats:            cfg.Stats,
		pluginRegistry:   cfg.PluginRegistry,
		mux:              http.NewServeMux(),
		sessions:         map[string]serverSession{},
		sessionPruneStop: make(chan struct{}),
		appConfig:        cfg.AppConfig,
		runtime:          cfg.Runtime,
		store:            cfg.Store,
	}
	server.mux.HandleFunc("/v1/responses", server.handleResponses)
	server.mux.HandleFunc("/responses", server.handleResponses)
	server.mux.HandleFunc("/v1/models", server.handleModels)
	server.mux.HandleFunc("/models", server.handleModels)
	go server.startSessionPruning()
	server.registerPluginRoutes()
	if cfg.Runtime != nil && cfg.Store != nil {
		apiRouter := api.NewRouter(server.store, server.runtime, server.stats, server.pluginRegistry, server)
		server.mux.Handle("/api/v1/", http.StripPrefix("/api/v1", apiRouter))
	}
	return server
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if token := server.currentConfig().AuthToken; token != "" {
		if !checkAuth(request, token) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(writer).Encode(openai.ErrorResponse{Error: openai.ErrorObject{
				Message: "未提供有效的认证令牌，请在 Authorization header 中使用 Bearer 方案",
				Type:    "authentication_error",
				Code:    "invalid_auth",
			}})
			return
		}
	}
	server.mux.ServeHTTP(writer, request)
}

func (server *Server) handleModels(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "仅支持 GET 请求",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}
	models := server.listModels()
	resp := struct {
		Models []codex.ModelInfo `json:"models"`
	}{
		Models: models,
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(resp)
}

func (server *Server) listModels() []codex.ModelInfo {
	return codex.BuildModelInfosFromConfig(server.currentConfig())
}

// currentConfig returns the effective configuration.
// When runtime is set (runtime mode), the runtime snapshot config takes priority.
// Otherwise, the static AppConfig is used.
func (server *Server) currentConfig() config.Config {
	if server.runtime != nil {
		return server.runtime.Current().Config
	}
	return server.appConfig
}


// CurrentConfig returns an interface for reading the effective config.
// Satisfies the api.ConfigAccessor interface expected by api.NewRouter.
func (server *Server) CurrentConfig() api.ConfigAccessor {
	return server
}
// AuthToken returns the current authentication token.
// Satisfies the api.ConfigAccessor interface.
func (server *Server) AuthToken() string {
	return server.currentConfig().AuthToken
}

// ListSessions returns snapshot info for all active sessions.
// Satisfies the server interface expected by api.NewRouter.
func (server *Server) ListSessions() []api.SessionInfo {
	server.sessionsMu.Lock()
	defer server.sessionsMu.Unlock()
	result := make([]api.SessionInfo, 0, len(server.sessions))
	for key, entry := range server.sessions {
		result = append(result, api.SessionInfo{
			Key:       key,
			CreatedAt: entry.sess.CreatedAt.Format(time.RFC3339),
			LastUsed:  entry.lastUsed.Format(time.RFC3339),
		})
	}
	return result
}

// onRequestCompleted dispatches a RequestCompletionHook event to all enabled
// plugins. No-op when the registry is nil or no plugins implement the hook.

// Only called after the request model is known (JSON parse succeeded).
// Early errors (bad method, read failure, decode failure) are not recorded.
func (server *Server) onRequestCompleted(model, actualModel string, startTime time.Time, usage plugin.RequestUsage, cost float64, status, errMsg string) {
	if server.pluginRegistry == nil {
		return
	}
	inputTokens := usage.NormalizedInputTokens
	outputTokens := usage.NormalizedOutputTokens
	cacheCreation := usage.NormalizedCacheCreation
	cacheRead := usage.NormalizedCacheRead
	server.pluginRegistry.OnRequestCompleted(
		&plugin.RequestContext{ModelAlias: model},
		plugin.RequestResult{
			Model:         model,
			ActualModel:   actualModel,
			InputTokens:   inputTokens,
			OutputTokens:  outputTokens,
			CacheCreation: cacheCreation,
			CacheRead:     cacheRead,
			Cost:          cost,
			Duration:      time.Since(startTime),
			Status:        status,
			ErrorMessage:  errMsg,
			Usage:         usage,
		},
	)
}

// registerPluginRoutes gives each RouteRegistrar plugin the opportunity to
// mount HTTP handlers on the server mux.
func (server *Server) registerPluginRoutes() {
	if server.pluginRegistry == nil {
		return
	}
	server.pluginRegistry.RegisterRoutes(func(pattern string, handler http.Handler) {
		server.mux.Handle(pattern, handler)
	})
}

func usageFromAnthropic(protocol string, source string, usage anthropic.Usage, inputIncludesCache bool) plugin.RequestUsage {
	raw := mustMarshalJSON(usage)
	normalizedInputTokens := anthropicNormalizedInputTokens(usage, inputIncludesCache)
	return plugin.RequestUsage{
		Protocol:    protocol,
		UsageSource: source,

		RawInputTokens:   usage.InputTokens,
		RawOutputTokens:  usage.OutputTokens,
		RawCacheCreation: usage.CacheCreationInputTokens,
		RawCacheRead:     usage.CacheReadInputTokens,

		NormalizedInputTokens:   normalizedInputTokens,
		NormalizedOutputTokens:  usage.OutputTokens,
		NormalizedCacheCreation: usage.CacheCreationInputTokens,
		NormalizedCacheRead:     usage.CacheReadInputTokens,

		RawUsageJSON: raw,
	}
}

func anthropicUsageFromStreamEvents(events []anthropic.StreamEvent) (anthropic.Usage, stats.BillingUsage, bool) {
	var usage anthropic.Usage
	var billing stats.BillingUsage
	inputIncludesCache := false
	for _, ev := range events {
		switch {
		case ev.Type == "message_start" && ev.Message != nil:
			if ev.Message.Usage.InputTokens > 0 {
				billing.FreshInputTokens = ev.Message.Usage.InputTokens
				billing.ProviderInputTokens = ev.Message.Usage.InputTokens
			}
			if ev.Message.Usage.OutputTokens > 0 {
				billing.OutputTokens = ev.Message.Usage.OutputTokens
			}
			if ev.Message.Usage.CacheCreationInputTokens > 0 {
				billing.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
			}
			if ev.Message.Usage.CacheReadInputTokens > 0 {
				billing.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
			}
			usage = mergeAnthropicUsage(usage, ev.Message.Usage)
		case ev.Type == "message_delta" && ev.Usage != nil:
			if streamInputIncludesCache(usage, *ev.Usage) {
				inputIncludesCache = true
				billing.FreshInputTokens = ev.Usage.InputTokens
				billing.ProviderInputTokens = usage.InputTokens
			} else if ev.Usage.InputTokens > 0 {
				billing.FreshInputTokens = ev.Usage.InputTokens
				billing.ProviderInputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.OutputTokens > 0 {
				billing.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				billing.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				billing.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
			usage = mergeAnthropicUsage(usage, *ev.Usage)
		}
	}
	if billing.ProviderInputTokens == 0 {
		billing.ProviderInputTokens = billing.InputTokens()
	}
	return usage, billing, inputIncludesCache
}

func mergeAnthropicUsage(current anthropic.Usage, updated anthropic.Usage) anthropic.Usage {
	if updated.InputTokens > 0 {
		if streamInputIncludesCache(current, updated) {
			// Some providers put total input on message_start, then fresh/cache
			// split on message_delta. Keep the total input while merging cache fields.
		} else {
			current.InputTokens = updated.InputTokens
		}
	}
	if updated.OutputTokens > 0 {
		current.OutputTokens = updated.OutputTokens
	}
	if updated.CacheCreationInputTokens > 0 {
		current.CacheCreationInputTokens = updated.CacheCreationInputTokens
	}
	if updated.CacheReadInputTokens > 0 {
		current.CacheReadInputTokens = updated.CacheReadInputTokens
	}
	return current
}

func streamInputIncludesCache(current anthropic.Usage, updated anthropic.Usage) bool {
	return updated.InputTokens > 0 &&
		current.InputTokens > updated.InputTokens &&
		current.CacheCreationInputTokens == 0 &&
		current.CacheReadInputTokens == 0 &&
		(updated.CacheCreationInputTokens > 0 || updated.CacheReadInputTokens > 0)
}

func statsUsageFromAnthropic(usage anthropic.Usage, inputIncludesCache bool) stats.Usage {
	return stats.Usage{
		InputTokens:              anthropicNormalizedInputTokens(usage, inputIncludesCache),
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
}

func billingUsageFromAnthropic(usage anthropic.Usage) stats.BillingUsage {
	return stats.BillingUsage{
		FreshInputTokens:         usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		ProviderInputTokens:      usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens,
	}
}

func anthropicNormalizedInputTokens(usage anthropic.Usage, inputIncludesCache bool) int {
	if inputIncludesCache {
		return usage.InputTokens
	}
	return usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
}

func usageFromStats(protocol string, source string, usage stats.Usage, rawUsage openai.Usage) plugin.RequestUsage {
	return plugin.RequestUsage{
		Protocol:    protocol,
		UsageSource: source,

		RawInputTokens:   usage.InputTokens,
		RawOutputTokens:  usage.OutputTokens,
		RawCacheCreation: usage.CacheCreationInputTokens,
		RawCacheRead:     usage.CacheReadInputTokens,

		NormalizedInputTokens:   usage.InputTokens,
		NormalizedOutputTokens:  usage.OutputTokens,
		NormalizedCacheCreation: usage.CacheCreationInputTokens,
		NormalizedCacheRead:     usage.CacheReadInputTokens,

		RawUsageJSON: mustMarshalJSON(rawUsage),
	}
}

func zeroUsage(protocol string, source string) plugin.RequestUsage {
	return plugin.RequestUsage{Protocol: protocol, UsageSource: source}
}

func mustMarshalJSON(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func (server *Server) handleResponses(writer http.ResponseWriter, request *http.Request) {
	log := slog.Default().With("path", request.URL.Path, "method", request.Method, "remote", request.RemoteAddr)
	log.Debug("收到请求")
	requestStart := time.Now()
	if request.Method != http.MethodPost {
		log.Warn("方法不允许", "method", request.Method)
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "方法不允许",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}

	sess := server.sessionForRequest(request)

	body, err := io.ReadAll(request.Body)
	record := mbtrace.Record{HTTPRequest: mbtrace.NewHTTPRequest(request), OpenAIRequest: mbtrace.RawJSONOrString(body)}
	if err != nil {
		log.Error("读取请求体失败", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "读取请求体失败",
			Type:    "invalid_request_error",
			Code:    "invalid_request_body",
		}}
		record.Error = traceError("read_openai_request", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	var responsesRequest openai.ResponsesRequest
	if err := json.Unmarshal(body, &responsesRequest); err != nil {
		log.Warn("无效的 JSON 请求体", "error", err)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "无效的 JSON 请求体",
			Type:    "invalid_request_error",
			Code:    "invalid_json",
		}}
		record.Error = traceError("decode_openai_request", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadRequest, payload)
		return
	}

	record.Model = responsesRequest.Model
	resolvedRoute, resolveErr := server.resolveModelOrFallback(responsesRequest.Model)
	if resolveErr != nil {
		log.Warn("请求了未知模型", "model", responsesRequest.Model)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("unknown model: %q", responsesRequest.Model),
			Type:    "invalid_request_error",
			Code:    "model_not_found",
		}}
		record.Error = traceError("model_not_found", fmt.Errorf("model %q not found", responsesRequest.Model))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusNotFound, payload)
		return
	}

	// Filter candidates by request features (e.g., image input).
	filteredCandidates, filterReason := server.filterCandidatesByInput(resolvedRoute.Candidates, responsesRequest.Input)
	if len(filteredCandidates) == 0 {
		log.Warn("过滤后无可用提供商", "model", responsesRequest.Model, "reason", filterReason)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no available provider for model %q with the requested features", responsesRequest.Model),
			Type:    "invalid_request_error",
			Code:    "provider_error",
		}}
		record.Error = traceError("provider_filtered", fmt.Errorf("candidates filtered: %s", filterReason))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		return
	}
	resolvedRoute.Candidates = filteredCandidates
	if filterReason != "" {
		log.Info("候选过滤", "model", responsesRequest.Model, "reason", filterReason)
	}

	// Protocol branch: get preferred candidate.
	preferred, ok := resolvedRoute.Preferred()
	if !ok {
		log.Error("模型解析结果无可用提供商", "model", responsesRequest.Model)
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no available provider for model %q", responsesRequest.Model),
			Type:    "server_error",
			Code:    "provider_error",
		}}
		record.Error = traceError("provider_error", fmt.Errorf("no available provider for %q", responsesRequest.Model))
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		return
	}

	if preferred.Protocol == config.ProtocolOpenAIResponse {
		server.handleOpenAIResponse(writer, request, responsesRequest, resolvedRoute.Candidates, record)
		return
	}

	// Resolve per-provider web search mode from the resolved route.
	reqOpts := server.resolveRequestOptions(responsesRequest.Model, resolvedRoute)

	anthropicRequest, plan, err := server.bridge.ToAnthropic(responsesRequest, sess.ExtensionData, reqOpts)
	conversionContext := server.bridge.ConversionContext(responsesRequest)
	record.AnthropicRequest = anthropicRequest
	if err != nil {
		log.Warn("转换为 Anthropic 格式失败", "error", err)
		status, payload := server.bridge.ErrorResponseForModel(responsesRequest.Model, err)
		record.Error = traceError("convert_to_anthropic", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		server.onRequestCompleted(
			responsesRequest.Model, "", requestStart,
			zeroUsage("anthropic", "none"), 0, "error", payload.Error.Message,
		)
		return
	}

	// Resolve the provider for this request using the ResolvedRoute (supports fallback chain).
	effectiveProvider := server.resolveProvider(responsesRequest.Model, resolvedRoute)
	if effectiveProvider == nil {
		log.Error("模型无可用提供商", "model", responsesRequest.Model)
		writeOpenAIError(writer, http.StatusBadGateway, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: fmt.Sprintf("no upstream provider configured for model %q", responsesRequest.Model),
			Type:    "server_error",
			Code:    "provider_error",
		}})
		server.onRequestCompleted(
			responsesRequest.Model, "", requestStart,
			zeroUsage("anthropic", "none"), 0, "error", fmt.Sprintf("no upstream provider: %s", responsesRequest.Model),
		)
		return
	}

	if responsesRequest.Stream {
		log.Debug("处理流式请求", "model", responsesRequest.Model)
		server.handleStream(writer, request, responsesRequest, anthropicRequest, plan, record, conversionContext, sess, effectiveProvider)
		return
	}
	log.Debug("发送非流式请求到提供商", "model", anthropicRequest.Model)
	anthropicResponse, err := effectiveProvider.CreateMessage(request.Context(), anthropicRequest)
	if err != nil {
		status, payload := server.bridge.ErrorResponseForModel(responsesRequest.Model, err)
		log.Error("请求失败",
			"request_model", responsesRequest.Model,
			"actual_model", anthropicRequest.Model,
			"status_code", status,
			"error", payload.Error.Message,
			"stage", "provider_create_message",
		)
		record.Error = traceError("provider_create_message", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		server.onRequestCompleted(
			responsesRequest.Model, anthropicRequest.Model, requestStart,
			zeroUsage("anthropic", "none"), 0, "error", payload.Error.Message,
		)
		return
	}

	openAIResponse := server.bridge.FromAnthropicWithPlanAndContext(anthropicResponse, responsesRequest.Model, plan, conversionContext, sess.ExtensionData)
	usage := anthropicResponse.Usage
	billingUsage := billingUsageFromAnthropic(usage)
	if server.stats != nil {
		server.stats.RecordBilling(responsesRequest.Model, anthropicRequest.Model, billingUsage)
	}
	logBillingUsageLine(responsesRequest.Model, anthropicRequest.Model, billingUsage, server.stats)
	record.AnthropicResponse = anthropicResponse
	record.OpenAIResponse = openAIResponse
	server.writeTrace(record)
	writeJSON(writer, http.StatusOK, openAIResponse)
	completionUsage := usageFromAnthropic("anthropic", "anthropic_response", usage, false)
	server.onRequestCompleted(
		responsesRequest.Model, anthropicRequest.Model, requestStart,
		completionUsage,
		func() float64 {
			if server.stats == nil {
				return 0
			}
			preferredCandidate, _ := resolvedRoute.Preferred()
			return computeCostWithProviderPricing(server.providerMgr, server.stats, responsesRequest.Model, anthropicRequest.Model, preferredCandidate.ProviderKey, billingUsage)
		}(),
		"success", "",
	)
}

func (server *Server) handleStream(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, anthropicRequest anthropic.MessageRequest, plan cache.CacheCreationPlan, record mbtrace.Record, context codex.ConversionContext, sess *session.Session, provider Provider) {
	log := slog.Default().With("model", responsesRequest.Model)
	log.Debug("开始流式传输")
	streamStart := time.Now()
	server.bridge.MarkCacheAttempt(plan)
	stream, err := provider.StreamMessage(request.Context(), anthropicRequest)
	if err != nil {
		status, payload := server.bridge.ErrorResponseForModel(responsesRequest.Model, err)
		log.Error("流式传输失败",
			"request_model", responsesRequest.Model,
			"actual_model", anthropicRequest.Model,
			"status_code", status,
			"error", payload.Error.Message,
			"stage", "provider_stream_message",
		)
		record.Error = traceError("provider_stream_message", err)
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, status, payload)
		server.onRequestCompleted(
			responsesRequest.Model, anthropicRequest.Model, streamStart,
			zeroUsage("anthropic", "none"), 0, "error", payload.Error.Message,
		)
		server.bridge.ResetCacheWarming(plan)
		return
	}
	defer stream.Close()

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)

	var events []anthropic.StreamEvent
	var streamErr string
	for {
		event, err := stream.Next()
		if err == io.EOF || (err != nil && err.Error() == "EOF") {
			break
		}
		if err != nil {
			events = append(events, anthropic.StreamEvent{Type: "error", Error: &anthropic.ErrorObject{Type: "provider_stream_error", Message: err.Error()}})
			record.Error = traceError("provider_stream_next", err)
			log.Error("流式读取错误", "error", err)
			streamErr = err.Error()
			break
		}
		events = append(events, event)
	}

	openAIEvents := server.bridge.ConvertStreamEventsWithContext(events, responsesRequest.Model, context, sess.ExtensionData, bridge.StreamOptions{
		PersistFinalTextReasoning: hasToolHistory(anthropicRequest.Messages),
	})
	record.AnthropicStreamEvents = events
	record.OpenAIStreamEvents = openAIEvents
	server.writeTrace(record)

	for _, event := range openAIEvents {
		writeSSE(writer, event)
	}
	usage, billingUsage, inputIncludesCache := anthropicUsageFromStreamEvents(events)
	if server.stats != nil {
		server.stats.RecordBilling(responsesRequest.Model, anthropicRequest.Model, billingUsage)
	}
	logBillingUsageLine(responsesRequest.Model, anthropicRequest.Model, billingUsage, server.stats)
	// Update cache registry from streaming usage signals.
	server.bridge.UpdateRegistryFromUsage(plan, cache.UsageSignals{
		InputTokens:              usage.InputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}, usage.InputTokens)
	completionUsage := usageFromAnthropic("anthropic", "anthropic_stream", usage, inputIncludesCache)
	server.onRequestCompleted(
		responsesRequest.Model, anthropicRequest.Model, streamStart,
		completionUsage,
		func() float64 {
			if server.stats == nil {
				return 0
			}
			return server.stats.ComputeBillingCost(responsesRequest.Model, billingUsage)
		}(),
		func() string {
			if streamErr != "" {
				return "error"
			}
			return "success"
		}(),
		streamErr,
	)
}

// resolveProvider selects the correct Provider for a given model alias.
// If a ProviderManager is configured, it uses it for routing.
// Otherwise it falls back to the single default provider.

func (server *Server) sessionForRequest(request *http.Request) *session.Session {
	key := sessionKeyFromRequest(request)
	if key == "" {
		sess := session.New()
		sess.InitExtensions(server.bridge.NewExtensionData())
		return sess
	}

	now := time.Now()
	server.sessionsMu.Lock()
	defer server.sessionsMu.Unlock()

	server.pruneSessionsLocked(now)
	if entry, ok := server.sessions[key]; ok {
		entry.lastUsed = now
		server.sessions[key] = entry
		return entry.sess
	}

	sess := session.NewWithID(key)
	sess.InitExtensions(server.bridge.NewExtensionData())
	server.sessions[key] = serverSession{sess: sess, lastUsed: now}
	return sess
}

func (server *Server) pruneSessionsLocked(now time.Time) {
	for key, entry := range server.sessions {
		if now.Sub(entry.lastUsed) > sessionTTL {
			delete(server.sessions, key)
		}
	}
}

func sessionKeyFromRequest(request *http.Request) string {
	if value := strings.TrimSpace(request.Header.Get("Session_id")); value != "" {
		return "session:" + value
	}
	if value := strings.TrimSpace(request.Header.Get("X-Codex-Window-Id")); value != "" {
		return "codex-window:" + value
	}
	return ""
}

func hasToolHistory(messages []anthropic.Message) bool {
	for _, message := range messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" || block.Type == "tool_result" {
				return true
			}
		}
	}
	return false
}

func (server *Server) writeTrace(record mbtrace.Record) {
	if server.tracer == nil || !server.tracer.Enabled() {
		return
	}
	requestNumber := server.tracer.NextRequestNumber()
	if shouldWriteResponseTrace(record) {
		server.writeTraceCategory("Response", requestNumber, mbtrace.Record{
			HTTPRequest:        record.HTTPRequest,
			OpenAIRequest:      record.OpenAIRequest,
			Model:              record.Model,
			OpenAIResponse:     record.OpenAIResponse,
			OpenAIStreamEvents: record.OpenAIStreamEvents,
			Error:              record.Error,
		})
	}
	if shouldWriteAnthropicTrace(record) {
		server.writeTraceCategory("Anthropic", requestNumber, mbtrace.Record{
			HTTPRequest:           record.HTTPRequest,
			AnthropicRequest:      record.AnthropicRequest,
			Model:                 record.Model,
			AnthropicResponse:     record.AnthropicResponse,
			AnthropicStreamEvents: record.AnthropicStreamEvents,
			Error:                 record.Error,
		})
	}
}

func (server *Server) writeTraceCategory(category string, requestNumber uint64, record mbtrace.Record) {
	if _, err := server.tracer.WriteNumbered(category, requestNumber, record); err != nil && server.traceErrors != nil {
		fmt.Fprintf(server.traceErrors, "跟踪 %s 写入失败: %v\n", category, err)
	}
}

func shouldWriteResponseTrace(record mbtrace.Record) bool {
	return record.OpenAIRequest != nil || record.OpenAIResponse != nil || record.OpenAIStreamEvents != nil
}

func shouldWriteAnthropicTrace(record mbtrace.Record) bool {
	return record.AnthropicRequest != nil || record.AnthropicResponse != nil || record.AnthropicStreamEvents != nil
}

func traceError(stage string, err error) map[string]string {
	return map[string]string{"stage": stage, "message": err.Error()}
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(payload)
}

func writeOpenAIError(writer http.ResponseWriter, status int, payload openai.ErrorResponse) {
	writeJSON(writer, status, payload)
}

func writeSSE(writer http.ResponseWriter, event openai.StreamEvent) {
	data, _ := json.Marshal(event.Data)
	_, _ = writer.Write([]byte("event: " + event.Event + "\n"))
	_, _ = writer.Write([]byte("data: " + string(data) + "\n\n"))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// anthropicClientWrapper adapts *anthropic.Client to the Provider interface.
type anthropicClientWrapper struct {
	client *anthropic.Client
}

func (w *anthropicClientWrapper) CreateMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.MessageResponse, error) {
	return w.client.CreateMessage(ctx, request)
}

func (w *anthropicClientWrapper) StreamMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.Stream, error) {
	return w.client.StreamMessage(ctx, request)
}

// handleOpenAIResponse proxies a request directly to an OpenAI Responses upstream
// without Anthropic protocol conversion. It handles both streaming and non-streaming.
// handleOpenAIResponse proxies a request directly to an OpenAI Responses upstream
// without Anthropic protocol conversion. It supports fallback across multiple
// OpenAI-response protocol candidates. Non-streaming: HTTP failure -> next candidate.
// Streaming: only fallback before SSE headers are written (during HTTP connect).
func (server *Server) handleOpenAIResponse(writer http.ResponseWriter, request *http.Request, responsesRequest openai.ResponsesRequest, candidates []provider.ProviderCandidate, record mbtrace.Record) {
	proxyStart := time.Now()
	var hookErr string
	var lastErr error
	actualModel := "" // updated with the successfully used upstream model
	defer func() {
		if hookErr != "" {
			server.onRequestCompleted(
				responsesRequest.Model, "", proxyStart,
				zeroUsage(config.ProtocolOpenAIResponse, "none"), 0, "error", hookErr,
			)
		}
	}()
	log := slog.Default().With("path", request.URL.Path, "method", request.Method)
	if server.providerMgr == nil {
		log.Error("未配置 OpenAI Responses 直通的提供商管理器")
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "提供商路由未配置",
			Type:    "server_error",
			Code:    "internal_error",
		}}
		record.Error = map[string]string{"stage": "openai_provider_config", "message": "provider manager not configured"}
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		hookErr = "provider manager not configured"
		return
	}

	// Filter to only OpenAI-response protocol candidates.
	openaiCandidates := make([]provider.ProviderCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Protocol == config.ProtocolOpenAIResponse {
			openaiCandidates = append(openaiCandidates, c)
		}
	}
	if len(openaiCandidates) == 0 {
		log.Error("没有 OpenAI Responses 协议的提供商候选")
		payload := openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "没有可用的提供商",
			Type:    "server_error",
			Code:    "provider_error",
		}}
		record.Error = map[string]string{"stage": "openai_provider_config", "message": "no openai-response candidates"}
		record.OpenAIResponse = payload
		server.writeTrace(record)
		writeOpenAIError(writer, http.StatusBadGateway, payload)
		hookErr = "no openai-response candidates"
		return
	}

	for i, candidate := range openaiCandidates {
		providerKey := candidate.ProviderKey
		isLast := i == len(openaiCandidates)-1
		log := logger.L().With("provider", providerKey, "attempt", i+1)

		baseURL := server.providerMgr.ProviderBaseURL(providerKey)
		apiKey := server.providerMgr.ProviderAPIKey(providerKey)
		if baseURL == "" {
			if isLast {
				log.Error("OpenAI 提供商缺少 base_url")
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "提供商未配置",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = map[string]string{"stage": "openai_provider_config", "message": "missing base_url"}
				record.OpenAIResponse = payload
				server.writeTrace(record)
				hookErr = "missing base_url"
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI 提供商缺少 base_url，尝试下一个候选",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1)
			lastErr = fmt.Errorf("provider %q has empty base_url", providerKey)
			continue
		}

		// Build upstream URL: baseURL + /v1/responses
		upstreamURL := strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(upstreamURL, "/v1/responses") && !strings.HasSuffix(upstreamURL, "/responses") {
			upstreamURL += "/v1/responses"
		}

		upstreamRequest := responsesRequest
		upstreamRequest.Model = candidate.UpstreamModel
		actualModel = candidate.UpstreamModel

		// Inject web_search tool if enabled for this model.
		if server.providerMgr.ResolvedWebSearchForModel(responsesRequest.Model) == "enabled" {
			upstreamRequest.Tools = InjectWebSearchTool(upstreamRequest.Tools)
		}

		body, err := json.Marshal(upstreamRequest)
		if err != nil {
			if isLast {
				log.Error("序列化请求失败", "error", err)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "内部错误",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = traceError("encode_openai_upstream_request", err)
				record.OpenAIResponse = payload
				hookErr = "encode upstream request"
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusInternalServerError, payload)
				return
			}
			logger.Warn("OpenAI 请求序列化失败，尝试下一个候选",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"error", err)
			lastErr = err
			continue
		}

		// Create upstream request
		upstreamReq, err := http.NewRequestWithContext(request.Context(), http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			if isLast {
				log.Error("创建上游请求失败", "error", err)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: "上游请求失败",
					Type:    "server_error",
					Code:    "internal_error",
				}}
				record.Error = traceError("create_openai_upstream_request", err)
				hookErr = "create upstream request"
				record.OpenAIResponse = payload
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI 上游请求创建失败，尝试下一个候选",
				"provider", providerKey,
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"error", err)
			lastErr = err
			continue
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)

		client := server.openAIHTTP
		if client == nil {
			client = &http.Client{Timeout: 0}
		}
		upstreamResp, err := client.Do(upstreamReq)
		if err != nil {
			if isLast {
				log.Error("OpenAI 上游请求失败",
					"request_model", responsesRequest.Model,
					"actual_model", upstreamRequest.Model,
					"error", err.Error(),
					"stage", "openai_upstream",
				)
				payload := openai.ErrorResponse{Error: openai.ErrorObject{
					Message: err.Error(),
					Type:    "server_error",
					Code:    "provider_error",
				}}
				hookErr = err.Error()
				record.Error = traceError("openai_upstream", err)
				record.OpenAIResponse = payload
				server.writeTrace(record)
				writeOpenAIError(writer, http.StatusBadGateway, payload)
				return
			}
			logger.Warn("OpenAI 上游连接失败，回退到下一个候选",
				"request_model", responsesRequest.Model,
				"attempt", i+1,
				"provider", providerKey,
				"error", err,
			)
			lastErr = err
			continue
		}
		defer upstreamResp.Body.Close()

		// Log successful fallback if not on the first candidate
		if i > 0 {
			logger.Info("OpenAI 回退成功",
				"request_model", responsesRequest.Model,
				"final_provider", providerKey,
				"final_model", candidate.UpstreamModel,
				"attempt", i+1,
			)
		}

		// Copy response headers and status
		for key, values := range upstreamResp.Header {
			for _, v := range values {
				writer.Header().Add(key, v)
			}
		}
		writer.WriteHeader(upstreamResp.StatusCode)

		traceEnabled := server.tracer != nil && server.tracer.Enabled()
		usageEnabled := upstreamResp.StatusCode >= 200 && upstreamResp.StatusCode <= 299 && (server.stats != nil || server.pluginRegistry != nil)
		shouldCapture := traceEnabled || usageEnabled

		var captured bytes.Buffer
		target := io.Writer(writer)
		if shouldCapture {
			target = io.MultiWriter(writer, &captured)
		}
		if _, err := io.Copy(target, upstreamResp.Body); err != nil {
			hookErr = "copy upstream response"
			log.Error("复制上游响应失败", "error", err)
			return
		}

		if traceEnabled {
			record.OpenAIResponse = mbtrace.RawJSONOrString(captured.Bytes())
			server.writeTrace(record)
		}

		// Capture usage for metrics recording.
		var billingUsage stats.BillingUsage
		var metricTelemetry plugin.RequestUsage
		if usageEnabled {
			if u, raw, source, ok := openAIUsageFromResponse(captured.Bytes(), responsesRequest.Stream); ok {
				billingUsage = u.BillingUsage()
				metricTelemetry = usageFromStats(config.ProtocolOpenAIResponse, source, u, raw)
				if server.stats != nil {
					server.stats.RecordBilling(responsesRequest.Model, actualModel, billingUsage)
					logBillingUsageLine(responsesRequest.Model, actualModel, billingUsage, server.stats)
				}
			}
		}
		if metricTelemetry.Protocol == "" {
			metricTelemetry = zeroUsage(config.ProtocolOpenAIResponse, "none")
		}

		// Record metrics via plugin hooks.
		status := "success"
		errMsg := ""
		if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
			status = "error"
			errMsg = fmt.Sprintf("HTTP %d", upstreamResp.StatusCode)
		}
		cost := float64(0)
		if server.stats != nil {
			cost = computeCostWithProviderPricing(server.providerMgr, server.stats, responsesRequest.Model, actualModel, providerKey, billingUsage)
		}
		server.onRequestCompleted(
			responsesRequest.Model, actualModel, proxyStart,
			metricTelemetry,
			cost, status, errMsg,
		)

		// Record trace including final provider info
		record.Model = fmt.Sprintf("%s (%s)", responsesRequest.Model, providerKey)

		return // success
	}

	// All candidates failed
	log.Error("所有 OpenAI Responses 提供商候选均失败",
		"request_model", responsesRequest.Model,
		"candidates_count", len(openaiCandidates),
		"last_error", lastErr,
	)
	if hookErr == "" {
		hookErr = fmt.Sprintf("all %d candidates failed: %v", len(openaiCandidates), lastErr)
	}
}
func logUsageLine(requestModel, actualModel string, usage stats.Usage, sessionStats *stats.SessionStats) {
	logBillingUsageLine(requestModel, actualModel, usage.BillingUsage(), sessionStats)
}

func logBillingUsageLine(requestModel, actualModel string, usage stats.BillingUsage, sessionStats *stats.SessionStats) {
	var requestCost float64
	var summary stats.Summary
	if sessionStats != nil {
		requestCost = sessionStats.ComputeBillingCost(requestModel, usage)
		summary = sessionStats.Summary()
	}
	rwRatio := stats.BillingCacheRWRatio(usage)
	slog.Info("请求完成",
		"request_model", requestModel,
		"actual_model", actualModel,
		"input_fresh", usage.FreshInputTokens,
		"input_cache_read", usage.CacheReadInputTokens,
		"input_cache_write", usage.CacheCreationInputTokens,
		"output_tokens", usage.OutputTokens,
		"request_cost", requestCost,
		"total_cost", summary.TotalCost,
		"cache_hit_rate", summary.CacheHitRate,
		"cache_write_rate", summary.CacheWriteRate,
		"cache_rw_ratio", rwRatio,
	)
}

func openAIUsageFromResponse(data []byte, stream bool) (stats.Usage, openai.Usage, string, bool) {
	if len(data) == 0 {
		return stats.Usage{}, openai.Usage{}, "", false
	}
	if stream {
		return openAIUsageFromSSE(data)
	}
	var payload struct {
		Usage openai.Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return stats.Usage{}, openai.Usage{}, "", false
	}
	usage, ok := statsUsageFromOpenAIUsage(payload.Usage)
	return usage, payload.Usage, "openai_response", ok
}

func openAIUsageFromSSE(data []byte) (stats.Usage, openai.Usage, string, bool) {
	var last stats.Usage
	var lastRaw openai.Usage
	found := false
	for _, event := range strings.Split(string(data), "\n\n") {
		var payload strings.Builder
		for _, line := range strings.Split(event, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				part := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if part == "" || part == "[DONE]" {
					continue
				}
				if payload.Len() > 0 {
					payload.WriteByte('\n')
				}
				payload.WriteString(part)
			}
		}
		if payload.Len() == 0 {
			continue
		}
		var envelope struct {
			Usage    openai.Usage `json:"usage"`
			Response struct {
				Usage openai.Usage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload.String()), &envelope); err != nil {
			continue
		}
		if usage, ok := statsUsageFromOpenAIUsage(envelope.Response.Usage); ok {
			last = usage
			lastRaw = envelope.Response.Usage
			found = true
			continue
		}
		if usage, ok := statsUsageFromOpenAIUsage(envelope.Usage); ok {
			last = usage
			lastRaw = envelope.Usage
			found = true
		}
	}
	return last, lastRaw, "openai_sse", found
}

func statsUsageFromOpenAIUsage(usage openai.Usage) (stats.Usage, bool) {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.InputTokensDetails.CachedTokens == 0 {
		return stats.Usage{}, false
	}
	cacheRead := usage.InputTokensDetails.CachedTokens
	freshInput := usage.InputTokens - cacheRead
	if freshInput < 0 {
		freshInput = 0
	}
	return stats.Usage{
		InputTokens:          usage.InputTokens,
		OutputTokens:         usage.OutputTokens,
		CacheReadInputTokens: cacheRead,
	}, true
}
func (server *Server) resolveProvider(modelAlias string, route *provider.ResolvedRoute) Provider {
	if server.providerMgr == nil {
		if server.provider != nil {
			return server.provider
		}
		return nil
	}

	if len(route.Candidates) == 0 {
		return nil
	}

	// Single candidate: use maybeWrapProvider as before (includes web search + visual wrapping).
	if len(route.Candidates) == 1 {
		c := route.Candidates[0]
		// When client is nil (legacy fallback from resolveModelOrFallback without providerMgr),
		// use the single fallback Provider.
		if c.Client == nil {
			return server.provider
		}
		return server.maybeWrapProvider(c.Client, modelAlias)
	}

	// Multi-candidate: filter same-protocol candidates, wrap preferred fully, fallback raw.
	prot := route.Candidates[0].Protocol
	sameProtocol := make([]provider.ProviderCandidate, 0, len(route.Candidates))
	for _, c := range route.Candidates {
		if c.Protocol == prot {
			sameProtocol = append(sameProtocol, c)
		}
	}
	if len(sameProtocol) == 0 {
		return server.maybeWrapProvider(route.Candidates[0].Client, modelAlias)
	}

	// Create fallbackProvider for raw-candidate retry, and fully-wrap the preferred one.
	fb := newFallbackProvider(&provider.ResolvedRoute{Candidates: sameProtocol}, server, modelAlias)
	preferredWrapped := server.maybeWrapProvider(sameProtocol[0].Client, modelAlias)
	if preferredWrapped == nil {
		return nil
	}
	// Composite: try preferred (fully wrapped) first, then fallbackProvider for raw client retry.
	return &wrappedFallbackProvider{
		preferred: preferredWrapped,
		fallback:  fb,
	}
}

func (server *Server) maybeWrapProvider(client *anthropic.Client, modelAlias string) Provider {
	var wrapped Provider = &anthropicClientWrapper{client: client}
	if server.providerMgr == nil {
		return server.maybeWrapVisual(wrapped, modelAlias)
	}
	resolved := server.providerMgr.ResolvedWebSearchForModel(modelAlias)
	if resolved == "injected" {
		tavilyKey := server.currentConfig().WebSearchTavilyKeyForModel(modelAlias)
		firecrawlKey := server.currentConfig().WebSearchFirecrawlKeyForModel(modelAlias)
		maxRounds := server.currentConfig().WebSearchMaxRoundsForModel(modelAlias)
		logger.L().Debug("包装注入式搜索编排器", "model", modelAlias)
		wrapped = websearchinjected.WrapProvider(client, tavilyKey, firecrawlKey, maxRounds)
	}
	return server.maybeWrapVisual(wrapped, modelAlias)
}

func (server *Server) maybeWrapVisual(provider Provider, modelAlias string) Provider {
	visualCfg, ok := visual.ConfigForModel(server.currentConfig(), modelAlias)
	if !ok {
		return provider
	}
	visualProvider := server.visualProvider(visualCfg)
	slog.Default().Debug("Wrapping Visual orchestrator", "model", modelAlias, "visual_model", visualCfg.Model)
	return visual.WrapProvider(provider, visualProvider, visualCfg.Model, visualCfg.MaxRounds, visualCfg.MaxTokens)
}

func (server *Server) visualProvider(cfg visual.Config) Provider {
	if server.providerMgr != nil && cfg.Provider != "" {
		client, err := server.providerMgr.ClientForKey(cfg.Provider)
		if err != nil {
			slog.Default().Warn("Visual provider unavailable", "provider", cfg.Provider, "error", err)
			return nil
		}
		return &anthropicClientWrapper{client: client}
	}
	return nil
}

// resolveRequestOptions builds per-request bridge options based on the provider's
func (server *Server) resolveRequestOptions(modelAlias string, route *provider.ResolvedRoute) bridge.RequestOptions {
	if server.providerMgr == nil {
		return bridge.RequestOptions{}
	}
	wsMode := server.providerMgr.ResolvedWebSearchForModel(modelAlias)
	if wsMode == "" {
		return bridge.RequestOptions{}
	}
	return bridge.RequestOptions{
		WebSearchMode:    wsMode,
		WebSearchMaxUses: server.currentConfig().WebSearchMaxUsesForModel(modelAlias),
		FirecrawlAPIKey:  server.currentConfig().WebSearchFirecrawlKeyForModel(modelAlias),
	}
}

// injectWebSearchTool adds a native web_search tool to the tools array if
// one is not already present. OpenAI Responses API supports this as a
// built-in tool type.
func InjectWebSearchTool(tools []openai.Tool) []openai.Tool {
	for _, t := range tools {
		if t.Type == "web_search" {
			return tools // already present
		}
	}
	if tools == nil {
		tools = make([]openai.Tool, 0, 1)
	}
	return append(tools, openai.Tool{Type: "web_search"})
}

// startSessionPruning runs a background goroutine that periodically
// cleans up expired sessions so they don't leak memory over time.
func (server *Server) startSessionPruning() {
	ticker := time.NewTicker(time.Hour) // prune every hour; sessionTTL is 24h
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			server.pruneSessions()
		case <-server.sessionPruneStop:
			return
		}
	}
}

// pruneSessions locks and prunes expired sessions.
func (server *Server) pruneSessions() {
	now := time.Now()
	server.sessionsMu.Lock()
	defer server.sessionsMu.Unlock()
	for key, entry := range server.sessions {
		if now.Sub(entry.lastUsed) > sessionTTL {
			delete(server.sessions, key)
		}
	}
}

// Close stops the background session pruning goroutine.
func (server *Server) Close() error {
	server.onceClose.Do(func() {
		close(server.sessionPruneStop)
	})
	return nil
}

func computeCostWithProviderPricing(pm *provider.ProviderManager, stats *stats.SessionStats, requestModel, actualModel, providerKey string, usage stats.BillingUsage) float64 {
	if stats == nil {
		return 0
	}
	// Try actual provider pricing first.
	if pm != nil {
		if meta, ok := pm.ModelMetaFor(actualModel, providerKey); ok {
			freshInput := float64(usage.FreshInputTokens)
			cacheWrite := float64(usage.CacheCreationInputTokens)
			cacheRead := float64(usage.CacheReadInputTokens)
			output := float64(usage.OutputTokens)
			cost := freshInput*meta.InputPrice/1000000 +
				cacheWrite*meta.CacheWritePrice/1000000 +
				cacheRead*meta.CacheReadPrice/1000000 +
				output*meta.OutputPrice/1000000
			if cost > 0 || meta.InputPrice > 0 || meta.OutputPrice > 0 {
				return cost
			}
		}
	}
	// Fall back to stats pricing map.
	return stats.ComputeBillingCost(requestModel, usage)
}

func checkAuth(r *http.Request, expectedToken string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return strings.TrimSpace(auth[7:]) == expectedToken
}

// resolveModelOrFallback resolves a model name to a ResolvedRoute.
// Uses ProviderManager.ResolveModel when available; falls back to bridge.ProviderFor
// for legacy single-provider mode.
func (server *Server) resolveModelOrFallback(modelName string) (*provider.ResolvedRoute, error) {
	if server.providerMgr != nil {
		return server.providerMgr.ResolveModel(modelName)
	}
	// Fallback: use bridge for route alias resolution (legacy single-provider mode).
	// When providerMgr is nil, we rely on the single fallback Provider (server.provider).
	providerKey := server.bridge.ProviderFor(modelName)
	if providerKey == "" && server.provider == nil {
		return nil, fmt.Errorf("no route or provider found for model %q", modelName)
	}
	// Return a synthetic ResolvedRoute with no client; resolveProvider will handle it.
	return &provider.ResolvedRoute{
		Candidates: []provider.ProviderCandidate{{
			ProviderKey: providerKey,
		}},
	}, nil
}

// wrappedFallbackProvider tries the preferred (fully-wrapped) provider first,
// then falls back to the raw-candidate fallback chain on failure.
type wrappedFallbackProvider struct {
	preferred Provider
	fallback  *fallbackProvider
}

func (w *wrappedFallbackProvider) CreateMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.MessageResponse, error) {
	resp, err := w.preferred.CreateMessage(ctx, request)
	if err == nil {
		return resp, nil
	}
	logger.Warn("首选提供商调用失败，回退到备选链", "error", err)
	return w.fallback.CreateMessage(ctx, request)
}

func (w *wrappedFallbackProvider) StreamMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.Stream, error) {
	stream, err := w.preferred.StreamMessage(ctx, request)
	if err == nil {
		return stream, nil
	}
	logger.Warn("首选提供商流式建连失败，回退到备选链", "error", err)
	return w.fallback.StreamMessage(ctx, request)
}

// fallbackProvider wraps multiple provider candidates and implements the Provider
// interface with automatic fallback. When the first candidate fails, it tries the
// next one (same protocol only) with a warning log. On stream, fallback only
// occurs before the first SSE header is written (i.e., during StreamMessage).
type fallbackProvider struct {
	candidates []provider.ProviderCandidate
	server     *Server
	modelAlias string
}

func newFallbackProvider(route *provider.ResolvedRoute, server *Server, modelAlias string) *fallbackProvider {
	return &fallbackProvider{
		candidates: route.Candidates,
		server:     server,
		modelAlias: modelAlias,
	}
}

func (f *fallbackProvider) CreateMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.MessageResponse, error) {
	var lastErr error
	for i, c := range f.candidates {
		request.Model = c.UpstreamModel
		resp, err := c.Client.CreateMessage(ctx, request)
		if err == nil {
			// Log successful fallback
			if i > 0 {
				logger.Info("回退成功",
					"request_model", f.modelAlias,
					"final_provider", c.ProviderKey,
					"final_model", c.UpstreamModel,
					"attempt", i+1,
				)
			}
			return resp, nil
		}
		lastErr = err
		if i < len(f.candidates)-1 {
			logger.Warn("提供商调用失败，切换到备选",
				"request_model", f.modelAlias,
				"attempt", i+1,
				"provider", c.ProviderKey,
				"error", err,
			)
		}
	}
	logger.Error("所有提供商候选均失败",
		"request_model", f.modelAlias,
		"last_error", lastErr,
	)
	return anthropic.MessageResponse{}, lastErr
}

func (f *fallbackProvider) StreamMessage(ctx context.Context, request anthropic.MessageRequest) (anthropic.Stream, error) {
	var lastErr error
	for i, c := range f.candidates {
		request.Model = c.UpstreamModel
		stream, err := c.Client.StreamMessage(ctx, request)
		if err == nil {
			if i > 0 {
				logger.Info("流式回退成功",
					"request_model", f.modelAlias,
					"final_provider", c.ProviderKey,
					"attempt", i+1,
				)
			}
			return stream, nil
		}
		lastErr = err
		if i < len(f.candidates)-1 {
			logger.Warn("提供商流式建连失败，切换到备选",
				"request_model", f.modelAlias,
				"attempt", i+1,
				"provider", c.ProviderKey,
				"error", err,
			)
		}
	}
	logger.Error("所有提供商流式候选均失败",
		"request_model", f.modelAlias,
		"last_error", lastErr,
	)
	return nil, lastErr
}

// requestHasImage checks whether the OpenAI request input contains image content.
// It scans the raw JSON for "input_image", "image", or "image_url" type fields.
func requestHasImage(input json.RawMessage) bool {
	if len(input) == 0 || string(input) == "null" {
		return false
	}
	var items []struct {
		Type string `json:"type"`
	}
	// Try array of content parts first.
	if err := json.Unmarshal(input, &items); err == nil {
		for _, it := range items {
			switch it.Type {
			case "input_image", "image", "image_url":
				return true
			}
		}
		return false
	}
	// Single string input has no image.
	return false
}

// filterCandidatesByInput filters a candidate list based on the actual request features:
// - Image input: filters out candidates whose InputModalities don't include "image".
// Returns the filtered slice and a log-friendly reason if any candidate was removed.
func (server *Server) filterCandidatesByInput(candidates []provider.ProviderCandidate, input json.RawMessage) ([]provider.ProviderCandidate, string) {
	if server.providerMgr == nil {
		return candidates, ""
	}

	hasImage := requestHasImage(input)
	if !hasImage {
		return candidates, "" // no image filtering needed
	}

	filtered := make([]provider.ProviderCandidate, 0, len(candidates))
	removedCount := 0
	for _, c := range candidates {
		meta, ok := server.providerMgr.ModelMetaFor(c.UpstreamModel, c.ProviderKey)
		if !ok || !hasModalityImage(meta.InputModalities) {
			removedCount++
			logger.L().Debug("过滤掉不支持图片的提供商候选", "provider", c.ProviderKey, "model", c.UpstreamModel)
			continue
		}
		filtered = append(filtered, c)
	}

	var reason string
	if removedCount > 0 {
		reason = fmt.Sprintf("请求包含图片输入，已过滤 %d 个不支持图片的提供商候选", removedCount)
	}
	return filtered, reason
}

// hasModalityImage checks if "image" is in the InputModalities list.
func hasModalityImage(modalities []string) bool {
	for _, m := range modalities {
		if m == "image" {
			return true
		}
	}
	return false
}
