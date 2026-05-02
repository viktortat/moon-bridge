// Package runtime provides a snapshot-based runtime that holds the active
// configuration, provider manager, and pricing data. The snapshot is
// updated atomically via an atomic.Pointer, allowing safe concurrent reads
// without locking.
package runtime

import (
	"fmt"
	"sync"
	"sync/atomic"

	"moonbridge/internal/foundation/config"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/stats"
)

// ConfigSnapshot is an immutable snapshot of the runtime state.
type ConfigSnapshot struct {
	// Config is the resolved runtime configuration.
	Config config.Config

	// ProviderMgr is the fully initialized provider manager.
	ProviderMgr *provider.ProviderManager

	// Pricing maps model identifiers to their pricing details.
	Pricing map[string]stats.ModelPricing
}

// Runtime holds the active ConfigSnapshot and provides atomic access
// and reload capability.
type Runtime struct {
	snapshot atomic.Pointer[ConfigSnapshot]
	mu       sync.Mutex // guards Reload; not needed for Current()
}

// NewRuntime creates a Runtime with the given initial configuration.
func NewRuntime(cfg config.Config, providerMgr *provider.ProviderManager, pricing map[string]stats.ModelPricing) *Runtime {
	rt := &Runtime{}
	snapshot := &ConfigSnapshot{
		Config:      cfg,
		ProviderMgr: providerMgr,
		Pricing:     pricing,
	}
	rt.snapshot.Store(snapshot)
	return rt
}

// Current returns the current ConfigSnapshot. The returned pointer is safe
// to use and will not be mutated by future Reload calls.
func (rt *Runtime) Current() *ConfigSnapshot {
	return rt.snapshot.Load()
}

// Reload validates the given config, builds a new ProviderManager, computes
// pricing, and atomically replaces the snapshot. Returns an error if
// validation or ProviderManager construction fails; the existing snapshot
// remains unchanged.
func (rt *Runtime) Reload(cfg config.Config) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	// Validate the new config.
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("runtime reload: config validation: %w", err)
	}

	// Build provider definitions and routes.
	providerDefs := buildProviderDefsFromConfig(cfg)
	modelRoutes := buildModelRoutesFromConfig(cfg)

	// Build new provider manager.
	providerMgr, err := provider.NewProviderManager(providerDefs, modelRoutes)
	if err != nil {
		return fmt.Errorf("runtime reload: provider manager: %w", err)
	}

	// Compute pricing from the new config.
	pricing := buildPricingFromConfig(cfg)

	// Create and atomically store the new snapshot.
	snapshot := &ConfigSnapshot{
		Config:      cfg,
		ProviderMgr: providerMgr,
		Pricing:     pricing,
	}
	rt.snapshot.Store(snapshot)
	return nil
}

// buildProviderDefsFromConfig converts config into provider definition map.
func buildProviderDefsFromConfig(cfg config.Config) map[string]provider.ProviderConfig {
	defs := make(map[string]provider.ProviderConfig, len(cfg.ProviderDefs))
	for key, def := range cfg.ProviderDefs {
		modelNames := make([]string, 0, len(def.Models))
		for name := range def.Models {
			modelNames = append(modelNames, name)
		}
		models := make(map[string]provider.ModelMeta, len(def.Models))
		for name, meta := range def.Models {
			models[name] = provider.ModelMeta(meta)
		}
		defs[key] = provider.ProviderConfig{
			BaseURL:          def.BaseURL,
			APIKey:           def.APIKey,
			Version:          def.Version,
			UserAgent:        def.UserAgent,
			Protocol:         def.Protocol,
			WebSearchSupport: string(def.WebSearchSupport),
			ModelNames:       modelNames,
			Models:           models,
			Offers:           def.Offers,
		}
	}
	return defs
}

// buildModelRoutesFromConfig converts config model entries into route definitions.
func buildModelRoutesFromConfig(cfg config.Config) map[string]provider.ModelRoute {
	routes := make(map[string]provider.ModelRoute, len(cfg.Routes))
	for alias, route := range cfg.Routes {
		routes[alias] = provider.ModelRoute{
			Provider: route.Provider,
			Name:     route.Model,
		}
	}
	return routes
}

// buildPricingFromConfig computes a pricing map from routes and provider models.
func buildPricingFromConfig(cfg config.Config) map[string]stats.ModelPricing {
	pricing := make(map[string]stats.ModelPricing)
	for alias, route := range cfg.Routes {
		if route.InputPrice > 0 || route.OutputPrice > 0 || route.CacheWritePrice > 0 || route.CacheReadPrice > 0 {
			pricing[alias] = stats.ModelPricing{
				InputPrice:      route.InputPrice,
				OutputPrice:     route.OutputPrice,
				CacheWritePrice: route.CacheWritePrice,
				CacheReadPrice:  route.CacheReadPrice,
			}
		}
	}
	for providerKey, def := range cfg.ProviderDefs {
		for modelName, meta := range def.Models {
			slug := providerKey + "/" + modelName
			newSlug := modelName + "(" + providerKey + ")"
			if _, exists := pricing[slug]; exists {
				if _, exists := pricing[newSlug]; !exists {
					pricing[newSlug] = pricing[slug]
				}
				continue
			}
			if meta.InputPrice > 0 || meta.OutputPrice > 0 || meta.CacheWritePrice > 0 || meta.CacheReadPrice > 0 {
				p := stats.ModelPricing{
					InputPrice:      meta.InputPrice,
					OutputPrice:     meta.OutputPrice,
					CacheWritePrice: meta.CacheWritePrice,
					CacheReadPrice:  meta.CacheReadPrice,
				}
				pricing[slug] = p
				pricing[newSlug] = p
				if _, exists := pricing[modelName]; !exists {
					pricing[modelName] = p
				}
			}
		}
	}
	return pricing
}
