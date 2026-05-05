package server

import (
	"encoding/json"
	"log/slog"
	"strings"

	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/service/stats"
)

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
