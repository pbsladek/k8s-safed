# Development

This repository pins local development tooling with mise.

```bash
mise trust .mise.toml
mise install
mise exec -- go version
make test
make lint
```

The pinned Go toolchain is declared in the repo root `.mise.toml`. Make targets
prefer that Go binary when mise is available, and CI continues to use
`actions/setup-go` from `go.mod`.

If your shell has a stale `GOROOT` or another `go` earlier in `PATH`, run
commands through the helper:

```bash
hack/dev-go.sh go test ./...
hack/dev-go.sh golangci-lint run ./...
hack/dev-go.sh make e2e-smoke
```

The helper exports:

- `GOROOT` from `mise where go`
- `PATH` with the mise Go `bin` directory first
- `SAFED_E2E_GO` so the e2e harness builds `kubectl-safed` with the same Go
  binary used by the test command
