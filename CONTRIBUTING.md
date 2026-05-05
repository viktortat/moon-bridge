# Contributing

## License

MoonBridge is licensed under GPL v3.

## Pull Requests

1. Fork the repository
2. Create a feature branch from `dev`
3. Make your changes
4. Run the full test suite: `CGO_ENABLED=0 go test ./... -count=1`
5. Ensure `CGO_ENABLED=0 go build ./...` passes
6. Open a pull request to the `dev` branch

## Development Setup

See [DEVELOPMENT.md](docs/DEVELOPMENT.md) for project structure and coding conventions.

## Code Style

- Go standard formatting (`gofmt`)
- Standard library `net/http` (no frameworks)
- Standard library `testing` (no external test frameworks)
- `CGO_ENABLED=0` compatible at all times
- `log/slog` for structured logging

## Testing

- All new code should include tests
- Use table-driven tests with Go's standard `testing` package
- Adapter tests (`protocol/*/adapter_test.go`) for protocol conversion logic
- Registry tests for registration/dispatch

## Project Phases

The project follows a phased roadmap in `.planning/ROADMAP.md`:

- **Phase 1**: Model layer decoupling — interface abstractions for model/provider/extension
- **Phase 2**: Internal format — Core intermediate types, Adapter interface, dual-segment bridge
- **Phase 3**: Tech debt cleanup + Extension migration — Bridge removal, Server decomposition, Extension migration to Core format

Each phase has artifacts in `.planning/phases/` including CONTEXT.md, RESEARCH.md, PLANS, and SUMMARIES.
