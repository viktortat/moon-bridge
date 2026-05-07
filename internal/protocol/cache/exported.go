package cache

// PlanCacheConfig holds all configuration needed for cache planning.
// Mirrors config.CacheConfig without importing the config package.
type PlanCacheConfig struct {
	Mode                     string
	TTL                      string
	PromptCaching            bool
	AutomaticPromptCache     bool
	ExplicitCacheBreakpoints bool
	AllowRetentionDowngrade  bool
	MaxBreakpoints           int
	MinCacheTokens           int
	ExpectedReuse            int
	MinimumValueScore        int
	MinBreakpointTokens      int
}

// PlannerConfig converts PlanCacheConfig to PlannerConfig with the given TTL.
func (cfg PlanCacheConfig) PlannerConfig(ttl string) PlannerConfig {
	if ttl == "" {
		ttl = cfg.TTL
	}
	return PlannerConfig{
		Mode:                     cfg.Mode,
		TTL:                      ttl,
		PromptCaching:            cfg.PromptCaching,
		AutomaticPromptCache:     cfg.AutomaticPromptCache,
		ExplicitCacheBreakpoints: cfg.ExplicitCacheBreakpoints,
		MaxBreakpoints:           cfg.MaxBreakpoints,
		MinCacheTokens:           cfg.MinCacheTokens,
		ExpectedReuse:            cfg.ExpectedReuse,
		MinimumValueScore:        cfg.MinimumValueScore,
		MinBreakpointTokens:      cfg.MinBreakpointTokens,
	}
}

// MessageBreakpointCandidate represents a potential cache breakpoint location
// within the message list, used by the planner to select optimal breakpoints.
type MessageBreakpointCandidate struct {
	MessageIndex int
	ContentIndex int
	BlockPath    string
	Hash         string
	Role         string
}

// CachePlanError is returned when cache planning fails due to a client error.
type CachePlanError struct {
	Status  int
	Message string
	Param   string
	Code    string
}

func (e *CachePlanError) Error() string {
	return e.Message
}

// UpdateRegistryFromUsage updates the in-memory cache registry from upstream usage signals.
func UpdateRegistryFromUsage(registry *MemoryRegistry, plan CacheCreationPlan, signals UsageSignals, inputTokens int) {
	if registry == nil {
		return
	}
	key := plan.PrefixKey
	if key == "" {
		key = plan.LocalKey
	}
	if key == "" {
		return
	}
	registry.UpdateFromUsage(key, signals, inputTokens, ParseTTL(plan.TTL))
}
