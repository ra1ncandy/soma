# Security Policy & Threat Model

`soma` is designed from the ground up around **environment-enforced security** rather than prompt-level willpower.

## 1. Defense-in-Depth Architecture

1. **Fail-Closed OS Sandboxing (`internal/sandbox`)**:
   - **Linux**: Uses `bubblewrap` (`bwrap --unshare-net --unshare-pid --die-with-parent --clearenv`) and probes Linux Landlock v1–v5 ABI availability. Pinned verification oracle files are bind-mounted read-only (`EROFS`) inside the sandbox so the agent cannot weaken tests to pass checks.
   - **macOS**: Generates deterministic Seatbelt SBPL profiles (`(deny network*)` by default, workspace-confined writes).
   - **Container & Remote Isolation**: Supports ephemeral rootless Docker (`--network=none`, `:ro` pinned oracle mounts) and non-interactive remote SSH (`BatchMode=yes`, `env -i`) with secret-scrubbed environments (`sandbox.ScrubEnv`).
2. **Secretless Capability Brokers (`internal/broker`)**:
   - Sandboxed tool executions receive **zero raw API keys, cloud tokens, or SSH private keys**.
   - Privileged external operations (`HTTPBroker`, `SSHBroker`, `FSBroker`) are invoked over host-validated requests where the host enforces domain/path allowlists, command prefix allowlists, remote mutual-exclusion leases (`remote_leases`), and JSON field constraints (such as forcing `{"draft": true}` on GitHub PR creations) before injecting credentials on the host side.
3. **Data/Action-Level Provenance Taint (`internal/taint`)**:
   - Grounded in *CaMeL* and *MCPTox* defenses against indirect prompt injection and Confused Deputy attacks.
   - Every content-addressed `@page_XXXX` virtual memory blob carries an explicit `trust_tier` (`trusted_user`, `trusted_repo`, or `untrusted_external` for web search, web extraction, and MCP responses).
   - Reading an `untrusted_external` `@page` does **not** lock down local `read_only` or sandboxed `workspace_write` exploration, but any `external_write` or `irreversible_write` referencing an untrusted `@page` is automatically blocked (`DecisionAsk` -> Operator Inbox) unless declassified via a deterministic schema parser (`taint.DeclassifyStructured`) or explicit operator approval.
4. **Anti-Poisoning Memory Quarantine (`internal/memory`)**:
   - Observations from `untrusted_external` sources cannot directly activate or supersede bi-temporal memory rules (*MINJA* defense); they are strictly quarantined with `status = 'proposed'` in the Operator Inbox (`soma inbox`).

## 2. Reporting a Vulnerability

If you discover a sandbox escape, provenance taint bypass, or broker policy vulnerability, please report it privately via GitHub Security Advisories on [`ra1ncandy/soma`](https://github.com/ra1ncandy/soma/security/advisories).
