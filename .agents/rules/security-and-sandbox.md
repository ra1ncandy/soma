# Security, Sandboxing & Provenance Taint Rules

`soma` relies on **environment-enforced security**, never prompt-level willpower. Preserve these invariants across `internal/sandbox`, `internal/broker`, `internal/taint`, `internal/policy`, `internal/memory`, and `internal/dag`:

1. **Fail-Closed OS Sandboxing**
   - Both synchronous `exec` and asynchronous `bg_exec` must execute inside the same fail-closed sandbox (`bwrap` with `--unshare-net --unshare-pid --unshare-ipc --unshare-uts --new-session --cap-drop ALL --die-with-parent --clearenv` on Linux; Seatbelt on macOS; ephemeral Docker or scrubbed remote SSH).
   - If no usable sandbox backend is available, shell execution **must return an error** — never silently fall back to unsandboxed host execution.
   - Note that Linux Landlock is currently probed by `soma init` only, not an enforcement backend.
   - Pinned oracle test files must remain bind-mounted read-only (`EROFS`) inside the sandbox so an agent cannot tamper with verification tests.

2. **Run-Wide Provenance Taint Parity (`internal/taint`, `internal/dag`, `internal/broker`)**
   - Every observation from an untrusted source (`web_search`, `web_extract`, MCP tools, untrusted broker reads) must be persisted as an `@page` tagged `untrusted_external` — even when small enough to display inline.
   - Once a run observes any `untrusted_external` page, every subsequent `external_write` in that run (in both `dag.Executor` and `broker.Router`) must union all run-wide untrusted pages into its provenance check, regardless of whether the model explicitly cites them.
   - Unknown/unrecognized tools (MCP tools, custom brokers) must default to `external_write`, never `read_only`.
   - Outbound reads (`web_search`, `web_extract`, `vision_analyze`, read-looking MCP tools) are `external_read`, never `read_only`: their arguments are an egress channel and are bounded by `taint.LowBandwidthEgress` once the run is tainted. MCP annotations may only tighten a classification (`destructiveHint: true` → `irreversible_write`).
   - Any action class other than `read_only`, `workspace_write` or `external_read` that was `dispatched` when the process died must become `ambiguous` and suspend for reconcile.
   - `irreversible_write` actions must always require approval. Tainted `external_write` actions must block (`DecisionAsk` / `ErrApprovalRequired`) until deterministically declassified via `taint.DeclassifyStructured` or explicitly approved in the Operator Inbox.

3. **Secretless Brokers & Outbound SSRF Guard**
   - Never leak host API keys, cloud tokens, or SSH private keys into sandboxed subprocess environments (always use `sandbox.ScrubEnv`).
   - All outbound web/HTTP tools (`web_extract`, `web_search`) must use `NewSSRFSafeHTTPClient` / `ValidateSSRFSafeURL` / `IsBlockedSSRFAddr` so dial-time and redirect-time checks block loopback, RFC-1918, link-local/cloud metadata, CGNAT, and DNS-rebound private addresses.

4. **Anti-Poisoning Memory & Skill Immutability**
   - Observations from `untrusted_external` sources must never directly activate or supersede bi-temporal memory rules (*MINJA* defense); they must stay quarantined as `status = 'proposed'` in the Operator Inbox.
   - Agent runs must never silently mutate `SKILL.md` files on disk.
