// Package format defines protocol-agnostic Core types for MoonBridge.
//
// This file defines the Registry for protocol Adapter registration and dispatch.
// The Registry is internal-use only and not exposed to the extension package.
package format

import (
	"fmt"
	"sync"
)

// Registry manages registration and lookup of protocol Adapters.
//
// It maintains separate maps for client-side (inbound) and provider-side (upstream)
// adapters of both synchronous and streaming varieties, ensuring clean separation
// between the two sides of the bridge.
//
// Thread-safe: all public methods are protected by sync.RWMutex.
//
// Internal use only — not exposed to extension packages.
type Registry struct {
	mu               sync.RWMutex
	clientAdapters   map[string]ClientAdapter
	providerAdapters map[string]ProviderAdapter
	clientStreams    map[string]ClientStreamAdapter
	providerStreams  map[string]ProviderStreamAdapter
}

// NewRegistry creates a new empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		clientAdapters:   make(map[string]ClientAdapter),
		providerAdapters: make(map[string]ProviderAdapter),
		clientStreams:    make(map[string]ClientStreamAdapter),
		providerStreams:  make(map[string]ProviderStreamAdapter),
	}
}

// ============================================================================
// Client Adapter Registration
// ============================================================================

// RegisterClient registers a ClientAdapter for an inbound protocol.
// Returns an error if an adapter for the same protocol is already registered.
func (r *Registry) RegisterClient(adapter ClientAdapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := adapter.ClientProtocol()
	if _, exists := r.clientAdapters[key]; exists {
		return fmt.Errorf("client adapter already registered for protocol: %s", key)
	}
	r.clientAdapters[key] = adapter
	return nil
}

// GetClient returns the ClientAdapter for the given inbound protocol identifier.
func (r *Registry) GetClient(protocol string) (ClientAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.clientAdapters[protocol]
	return a, ok
}

// ============================================================================
// Provider Adapter Registration
// ============================================================================

// RegisterProvider registers a ProviderAdapter for an upstream protocol.
// The key must align with ProviderCandidate.Protocol (e.g. "anthropic").
// Returns an error if an adapter for the same protocol is already registered.
func (r *Registry) RegisterProvider(adapter ProviderAdapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := adapter.ProviderProtocol()
	if _, exists := r.providerAdapters[key]; exists {
		return fmt.Errorf("provider adapter already registered for protocol: %s", key)
	}
	r.providerAdapters[key] = adapter
	return nil
}

// GetProvider returns the ProviderAdapter for the given upstream protocol identifier.
func (r *Registry) GetProvider(protocol string) (ProviderAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.providerAdapters[protocol]
	return a, ok
}

// ============================================================================
// Client Stream Registration
// ============================================================================

// RegisterClientStream registers a ClientStreamAdapter for an inbound protocol.
// Returns an error if a stream adapter for the same protocol is already registered.
func (r *Registry) RegisterClientStream(adapter ClientStreamAdapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := adapter.ClientProtocol()
	if _, exists := r.clientStreams[key]; exists {
		return fmt.Errorf("client stream adapter already registered for protocol: %s", key)
	}
	r.clientStreams[key] = adapter
	return nil
}

// GetClientStream returns the ClientStreamAdapter for the given inbound protocol identifier.
func (r *Registry) GetClientStream(protocol string) (ClientStreamAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.clientStreams[protocol]
	return s, ok
}

// ============================================================================
// Provider Stream Registration
// ============================================================================

// RegisterProviderStream registers a ProviderStreamAdapter for an upstream protocol.
// Returns an error if a stream adapter for the same protocol is already registered.
func (r *Registry) RegisterProviderStream(adapter ProviderStreamAdapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := adapter.ProviderProtocol()
	if _, exists := r.providerStreams[key]; exists {
		return fmt.Errorf("provider stream adapter already registered for protocol: %s", key)
	}
	r.providerStreams[key] = adapter
	return nil
}

// GetProviderStream returns the ProviderStreamAdapter for the given upstream protocol identifier.
func (r *Registry) GetProviderStream(protocol string) (ProviderStreamAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.providerStreams[protocol]
	return s, ok
}

// ============================================================================
// Protocol Listing
// ============================================================================

// ProviderProtocols returns the list of all registered upstream protocol identifiers.
func (r *Registry) ProviderProtocols() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.providerAdapters))
	for k := range r.providerAdapters {
		keys = append(keys, k)
	}
	return keys
}

// ClientProtocols returns the list of all registered inbound protocol identifiers.
func (r *Registry) ClientProtocols() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	keys := make([]string, 0, len(r.clientAdapters))
	for k := range r.clientAdapters {
		keys = append(keys, k)
	}
	return keys
}
