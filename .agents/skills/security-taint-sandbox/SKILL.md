---
name: security-taint-sandbox
description: >-
  Guide for modifying OS sandboxing (internal/sandbox), provenance taint tracking (internal/taint),
  secretless capability brokers (internal/broker), SSRF protection (internal/dag/web_multimodal.go),
  memory anti-poisoning quarantine (internal/memory), and EROFS oracle pinning (internal/oracle).
  Use when touching any security boundary, sandbox backend, taint rule, broker, or memory trust tier.
---

# Security, Sandboxing & Provenance Taint

Read `SECURITY.md` before changing any security boundary.

## Key Files

- `internal/sandbox/` — `bwrap_linux.go`, `landlock_linux.go` (probe-only), `seatbelt.go`, `remote_backends.go`, `sandbox.go` (`ScrubEnv`)
- `internal/taint/taint.go` — Trust tiers (`trusted_user`, `trusted_repo`, `untrusted_external`), `DeclassifyStructured`
- `internal/policy/policy.go` — Effect classes and decision evaluation
- `internal/broker/` — `broker.go` (`Router`), `http_broker.go`, `ssh_broker.go`, `fs_broker.go`
- `internal/dag/web_multimodal.go` — `NewSSRFSafeHTTPClient`, `ValidateSSRFSafeURL`, `IsBlockedSSRFAddr`
- `internal/memory/memory.go` — Bi-temporal graph, `QueryAsOf`, *MINJA* untrusted quarantine (`status = 'proposed'`)
- `internal/oracle/` — `CommandOracle` (`EROFS` read-only pinned tests), `ArtifactOracle`, `AdvisoryReviewer`

## Invariants Checklist

1. **Fail-Closed Execution**: Never allow `exec` or `bg_exec` to fall back to unsandboxed host execution when `bwrap` / Seatbelt / Docker / SSH is unavailable.
2. **Environment Scrubbing**: Always apply `sandbox.ScrubEnv` so host credentials never enter sandboxed children.
3. **Run-Wide Taint Parity**: Any change to taint enforcement in `dag.Executor` must be mirrored in `broker.Router` (and vice versa). Every `untrusted_external` observation must persist as an `@page`, even if small enough to inline.
4. **Default-Strict Tool Classification**: Unrecognized tools (MCP, custom brokers) must classify as `external_write`, not `read_only`.
5. **Dial-Time & Redirect-Time SSRF Checks**: Any new HTTP fetch path must use `NewSSRFSafeHTTPClient` so redirects and DNS rebinding cannot reach loopback, RFC-1918, CGNAT, or cloud metadata endpoints.
6. **Oracle Integrity**: Keep pinned test paths mounted read-only (`EROFS`) in the sandbox and verify stale checksums are rejected.

## Validation

```bash
go test -count=1 -race -v ./internal/sandbox/... ./internal/taint/... ./internal/broker/... ./internal/policy/... ./internal/memory/... ./internal/oracle/... ./internal/skills/...
go test -count=1 -race -v -run "TestB3|TestB4|TestB5" ./benchmarks/...
```

Also run `internal/dag/sandbox_gaps_test.go`:

```bash
go test -count=1 -race -v -run TestSandbox ./internal/dag/...
```
