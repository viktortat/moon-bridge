package app

import (
	"log/slog"
	"testing"

	"moonbridge/internal/config"
	"moonbridge/internal/extension/deepseek_v4"
)

func ptrBool(v bool) *bool { return &v }

// TestDeepSeekPlugin_EnabledForModelViaResolver verifies that the isEnabled
// resolver passed to deepseekv4.NewPlugin correctly delegates to
// config.ExtensionEnabled, which resolves "enabled: true" from model-level
// config through the ProviderDefs catalog.
func TestDeepSeekPlugin_EnabledForModelViaResolver(t *testing.T) {
	// Simulate the config structure used in production:
	// a provider offering a model with deepseek_v4 extension enabled.
	cfg := config.Config{
		ProviderDefs: map[string]config.ProviderDef{
			"test-provider": {
				Protocol: "anthropic",
				BaseURL:  "https://test.example.com",
				Models: map[string]config.ModelMeta{
					"enabled-model": {
						Extensions: map[string]config.ExtensionSettings{
							"deepseek_v4": {Enabled: ptrBool(true)},
						},
					},
					"disabled-model": {
						Extensions: map[string]config.ExtensionSettings{
							"deepseek_v4": {Enabled: ptrBool(false)},
						},
					},
				},
			},
		},
	}

	reg := BuiltinExtensions().NewRegistry(slog.Default(), cfg)
	if reg == nil {
		t.Fatal("NewRegistry returned nil")
	}

	p := reg.Plugin(deepseekv4.PluginName)
	if p == nil {
		t.Fatalf("plugin %q not found", deepseekv4.PluginName)
	}

	if !p.EnabledForModel("enabled-model") {
		t.Error("EnabledForModel('enabled-model') = false, want true")
	}
	if p.EnabledForModel("disabled-model") {
		t.Error("EnabledForModel('disabled-model') = true, want false")
	}
	if p.EnabledForModel("unknown-model") {
		t.Error("EnabledForModel('unknown-model') = true, want false")
	}
}
