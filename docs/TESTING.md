# Testing

MoonBridge uses Go's standard library `testing` package exclusively. No external test frameworks (testify, gomega, etc.).

## Running Tests

```bash
# All packages
go test ./... -count=1

# Specific package
go test ./internal/protocol/format/ -count=1 -v

# Named test
go test ./internal/protocol/anthropic/ -run TestFromCoreRequest -count=1 -v

# With coverage
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

## Test Categories

| Category | Location | Description |
|----------|----------|-------------|
| Unit tests | Per-package `*_test.go` | Table-driven tests for individual functions |
| DTO tests | `protocol/*/dto_test.go` | JSON serialization round-trips |
| Adapter tests | `protocol/*/adapter_test.go` | Protocol ↔ Core conversion (non-streaming + streaming) |
| Registry tests | `protocol/format/registry_test.go` | Adapter register/lookup lifecycle |
| Server tests | `service/server/server_test.go` | HTTP handler + auth + integration |
| API e2e tests | `service/api/*_test.go` | Management API endpoint tests |
| Provider tests | `service/provider/*_test.go` | Provider manager + routing |
| Extension tests | `extension/*/*_test.go` | Plugin-specific tests (deepseek_v4, visual, websearch, etc.) |
| Store tests | `service/store/*_test.go` | ConfigStore persistence |
| CLI tests | `cmd/moonbridge/*_test.go` | CLI flag parsing |

## Phase 2/3 Core Format Tests

The protocol adapter layer has dedicated test coverage:

- **`internal/protocol/format/registry_test.go`** (15 tests): Registry CRUD, duplicate detection, stream adapter registration
- **`internal/protocol/anthropic/adapter_test.go`** (18 tests): `FromCoreRequest` (12), `ToCoreResponse` (6) — text, system, tools, tool_choice, image, reasoning, thinking
- **`internal/protocol/openai/adapter_test.go`** (5 tests): `ToCoreRequest` (basic text, instructions, nil input), `FromCoreResponse` (basic, error)

## Writing Tests

```go
func TestSomething(t *testing.T) {
    tests := []struct{
        name string
        input InputType
        want  ExpectedType
    }{
        {name: "basic case", input: val1, want: val2},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := FunctionUnderTest(tt.input)
            if got != tt.want {
                t.Errorf("got %v, want %v", got, tt.want)
            }
        })
    }
}
```

## CGO Requirement

All tests must pass with `CGO_ENABLED=0`:

```bash
CGO_ENABLED=0 go test ./... -count=1
```

This ensures compatibility with the pure-Go SQLite driver and WASM build target.
