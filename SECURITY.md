# Security Policy & Threat Model

`soma` is designed from the ground up around **environment-enforced security** rather than prompt-level willpower.

## 1. Defense-in-Depth Architecture

1. **Fail-Closed OS Sandboxing (`internal/sandbox`)**:
   - **Linux**: Uses `bubblewrap` (`bwrap --unshare-net --unshare-pid --unshare-ipc --unshare-uts --new-session --cap-drop ALL --die-with-parent --clearenv`) for both `exec` and `bg_exec`. With no usable backend, shell tools return an error instead of running on the host. Landlock ABI availability is probed and reported by `soma init`, but Landlock is **not** an enforcement backend yet: without `bwrap`, Linux has no sandbox and shell tools are refused. Pinned verification oracle files are bind-mounted read-only (`EROFS`) inside the sandbox so the agent cannot weaken tests to pass checks.
   - **macOS**: Generates deterministic Seatbelt SBPL profiles (`(deny network*)` by default, workspace-confined writes).
   - **Container & Remote Isolation**: Supports ephemeral rootless Docker (`--network=none`, `:ro` pinned oracle mounts) and non-interactive remote SSH (`BatchMode=yes`, `env -i`) with secret-scrubbed environments (`sandbox.ScrubEnv`).
2. **Secretless Capability Brokers (`internal/broker`)**:
   - Sandboxed tool executions receive **zero raw API keys, cloud tokens, or SSH private keys**.
   - Privileged external operations (`HTTPBroker`, `SSHBroker`, `FSBroker`) are invoked over host-validated requests where the host enforces domain/path allowlists, command prefix allowlists, remote mutual-exclusion leases (`remote_leases`), and JSON field constraints (such as forcing `{"draft": true}` on GitHub PR creations) before injecting credentials on the host side.
3. **Data/Action-Level Provenance Taint (`internal/taint`)**:
   - Grounded in *CaMeL* and *MCPTox* defenses against indirect prompt injection and Confused Deputy attacks.
   - Every content-addressed `@page_XXXX` virtual memory blob carries an explicit `trust_tier` (`trusted_user`, `trusted_repo`, or `untrusted_external` for web search, web extraction, and MCP responses).
   - Every untrusted observation is recorded as an `@page`, including ones small enough to stay inline. Reading untrusted content does **not** lock down local `read_only` or sandboxed `workspace_write` exploration, but from then on every `external_write` in that run is treated as referencing all of the run's untrusted pages, whether or not the model cites them, and any `irreversible_write` always asks. Tools the kernel does not recognise (MCP tools, brokers) are classed `external_write`, not `read_only`. Such an action is automatically blocked (`DecisionAsk` -> Operator Inbox) unless declassified via a deterministic schema parser (`taint.DeclassifyStructured`) or explicit operator approval.
4. **Anti-Poisoning Memory Quarantine (`internal/memory`)**:
   - Observations from `untrusted_external` sources cannot directly activate or supersede bi-temporal memory rules (*MINJA* defense); they are strictly quarantined with `status = 'proposed'` in the Operator Inbox (`soma inbox`).

## 2. Known Limitations

- **No seccomp filter or resource limits** on the `bwrap` sandbox yet, and no user-namespace remap: a kernel exploit or a fork bomb inside the sandbox is out of scope today.
- **Taint is tracked per run, not per file.** Untrusted content written into the workspace in one run is read back as `trusted_repo` by a later run.
- **macOS Seatbelt profile is allow-by-default** with targeted denies, which is weaker than the Linux mount-namespace model.
- **Capability brokers take provenance from their caller.** `broker.Router` checks only the page IDs it is handed; it does not yet apply the run-wide rule above.

## 3. Reporting a Vulnerability

If you discover a sandbox escape, provenance taint bypass, or broker policy vulnerability, please report it privately via GitHub Security Advisories on [`ra1ncandy/soma`](https://github.com/ra1ncandy/soma/security/advisories).
