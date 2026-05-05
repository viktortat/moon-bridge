package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"moonbridge/internal/extension/codex"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/extension/visual"
	"moonbridge/internal/extension/websearchinjected"
	"moonbridge/internal/foundation/config"
	"moonbridge/internal/foundation/logger"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/format"
	"moonbridge/internal/service/api"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/runtime"
	"moonbridge/internal/service/stats"
	"moonbridge/internal/service/store"

	mbtrace "moonbridge/internal/service/trace"
)

// Provider defines the upstream interface for creating messages.
type Provider interface {
	CreateMessage(context.Context, anthropic.MessageRequest) (anthropic.MessageResponse, error)
	StreamMessage(context.Context, anthropic.MessageRequest) (anthropic.Stream, error)
}

type Config struct {
	AdapterRegistry *format.Registry // adapter dispatch path (format registry)
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
	adapterRegistry  *format.Registry
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



func New(cfg Config) *Server {
	server := &Server{
		adapterRegistry:  cfg.AdapterRegistry,
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

// onRequestCompleted dispatches a RequestCompletionHook event to all enabled
// plugins. No-op when the registry is nil or no plugins implement the hook.

// Only called after the request model is known (JSON parse succeeded).
// Early errors (bad method, read failure, decode failure) are not recorded.

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

// pruneSessions locks and prunes expired sessions.

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
// Uses ProviderManager.ResolveModel; requires providerMgr to be set.
func (server *Server) resolveModelOrFallback(modelName string) (*provider.ResolvedRoute, error) {
	if server.providerMgr != nil {
		return server.providerMgr.ResolveModel(modelName)
	}
	// No provider manager configured. Cannot resolve model.
	return nil, fmt.Errorf("no provider manager configured for model %q", modelName)

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
