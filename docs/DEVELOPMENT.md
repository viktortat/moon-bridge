# Development

## Prerequisites

- Go 1.25+
- CGO_ENABLED=0 (static compilation, pure-Go SQLite)
- Optional: pnpm (for Cloudflare Worker dev)
- Optional: Python 3 (for migration/analytics scripts)

## Quick Start

```bash
git clone <repo> && cd moonbridge
go mod download
cp config.example.yml "${XDG_CONFIG_HOME:-$HOME/.config}/moonbridge/config.yml"
# edit config.yml with at least one provider's api_key
go run ./cmd/moonbridge
```

## Project Structure

```
cmd/
  moonbridge/main.go     # Standalone server entry
  cloudflare/main.go     # Cloudflare Workers WASM entry

internal/
  foundation/            # Shared infrastructure
    config/              # YAML config loading + parsing
    db/                  # DB Provider/Consumer/Registry abstraction
    openai/              # OpenAI Responses DTO (migrated to protocol/openai)
    logger/              # Structured logger (slog)
    session/             # Session state
    modelref/            # Model reference parsing

  protocol/              # Protocol layer
    format/              # Core intermediate format (protocol-agnostic types)
    anthropic/           # Anthropic Messages client + adapter
    openai/              # OpenAI Responses types + adapter
    cache/               # Prompt cache planning

  service/               # Application layer
    server/              # HTTP server, dispatch, session, usage
    app/                 # Bootstrap wiring
    provider/            # Multi-provider management + routing
    api/                 # Management REST API
    runtime/             # Atomic config hot-reload
    store/               # ConfigStore persistence
    proxy/               # CaptureResponse/CaptureAnthropic proxy
    stats/               # Session stats
    trace/               # Request tracing

  extension/             # Plugin system
    plugin/              # Plugin interfaces + registry
    deepseek_v4/         # DeepSeek V4 adapter
    websearch/           # Web search orchestration
    websearchinjected/   # Injected web search provider wrapper
    visual/              # Visual extension provider wrapper
    codex/               # Codex model catalog + helpers
    metrics/             # Usage metrics persistence
    db/                  # DB extensions (sqlite, d1)
```

## Architecture Overview

The request flow after Phase 3 (Bridge removed, Adapter-only):

```
Client (OpenAI Responses)
    ↓
OpenAIClientAdapter.ToCoreRequest()
    ↓
CoreRequest (protocol-agnostic intermediate format)
    ↓
AnthropicProviderAdapter.FromCoreRequest() + CacheManager
    ↓
Upstream API call → Anthropic MessageResponse
    ↓
AnthropicProviderAdapter.ToCoreResponse()
    ↓
CoreResponse
    ↓
OpenAIClientAdapter.FromCoreResponse()
    ↓
Client receives OpenAI Response
```

Streaming follows the same path via `CoreStreamEvent` channels.

## Coding Conventions

- `CGO_ENABLED=0` at all times (pure-Go SQLite, WASM compatibility)
- Standard library `net/http` for HTTP — no frameworks
- Standard library `testing` — no external test frameworks
- `log/slog` for structured logging
- `sync/atomic` for lock-free concurrent reads (see `runtime.Runtime`)

## Commands

```bash
go build ./...           # Build all packages
go vet ./...             # Static analysis
go test ./... -count=1   # Full test suite
make test                # Test with coverage
make cover               # Coverage report
```
