# E2E Improvement Implementation Plan

This file tracks the current e2e hardening phase.

- [x] Add e2e tiers and category Make targets.
- [x] Emit clearer progress logs before long e2e tests and setup phases.
- [x] Capture diagnostics into an artifact directory from the test harness.
- [x] Add RBAC variants for missing workload patch, pod eviction, and event create.
- [x] Add deletion-race coverage for workload and pod disappearance.
- [x] Add corrupt checkpoint resume coverage.
- [x] Add config environment-variable precedence coverage.
- [x] Add an e2e coverage matrix to `e2e/README.md`.
- [x] Run formatting, unit tests, e2e compile, smoke e2e, and focused new-case e2e.

Verification notes:

- Unit tests and e2e compile were run with `/Users/pbsladek/.local/share/mise/installs/go/1.26.2/bin/go`.
- `make e2e-smoke` passed with the matching Go binary first in `PATH`.
- Focused e2e coverage for the new config/checkpoint/RBAC/deletion-race tests passed after fixing the config parser and stale-owner assertion.
- `golangci-lint run ./...` is blocked locally by a Go toolchain mismatch: `/opt/homebrew/bin/go` reports 1.26.3 while `GOROOT` points at a 1.26.2 tree, and the Homebrew 1.26.3 stdlib path appears incomplete in this shell.
