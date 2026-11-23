---
name: verify-and-build
description: >-
  Run the full local and CI verification suite for soma: static analysis, race-detector unit
  tests and B1-B6 conformance suites, cross-platform CGO_ENABLED=0 static builds, and Suite B6
  shadow report regeneration. Use before committing, opening a PR, or validating kernel changes.
---

# Verify and Build `soma`

Use this runbook to match `.github/workflows/ci.yml` and `CONTRIBUTING.md` locally.

## 1. Static Analysis & Full Race Test Suite

```bash
go vet ./...
go test -count=1 -race ./...
```

This runs all unit tests across `internal/...` plus the offline B1–B6 conformance suites (`benchmarks/`), `live_baseline_test.go`, and `live_hard_offline_test.go`. Live LLM tests are automatically skipped unless `SOMA_LIVE_API_KEY` or `OPENCODE_API_KEY` is set.

### Targeted Package / Suite Runs

- Single package: `go test -count=1 -race -v ./internal/<pkg>/...`
- Specific conformance suite: `go test -count=1 -race -v -run TestB4 ./benchmarks/...`

### Linux `bwrap` Troubleshooting

If sandbox tests fail on Ubuntu/AppArmor hosts with unprivileged user namespace errors:

```bash
sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
```

Verify `bubblewrap`, `git`, and `python3` are installed (`which bwrap git python3`).

## 2. Static Pure-Go Builds (`CGO_ENABLED=0`)

Build local stripped static binaries (`bin/soma` and `bin/soma-bridge-discord`):

```bash
make build
```

Verify cross-platform static compilation (matches CI):

```bash
for target in "linux/amd64" "linux/arm64" "darwin/amd64" "darwin/arm64"; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags="-s -w" -o "/tmp/soma-$GOOS-$GOARCH" ./cmd/soma
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags="-s -w" -o "/tmp/soma-bridge-discord-$GOOS-$GOARCH" ./cmd/soma-bridge-discord
done
```

## 3. Regenerate Suite B6 Conformance Report

Whenever `internal/shadow`, `internal/compiler`, `internal/dag`, or conformance scenarios change, regenerate `docs/benchmarks/BENCHMARK_REPORT.md`:

```bash
make shadow
```

Inspect `git diff docs/benchmarks/BENCHMARK_REPORT.md` to confirm all 4 scenarios pass with the kernel mechanism enabled (`4/4`) and fail when disabled (`0/4`), with `100%` `ModeExact` replay.
