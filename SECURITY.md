# Security Policy & Threat Model

`soma` is designed from the ground up around **environment-enforced security** rather than prompt-level willpower.

## 1. Defense-in-Depth Architecture

1. **Fail-Closed OS Sandboxing (`internal/sandbox`)**:
   - **Linux**: Uses `bubblewrap` (`bwrap --unshare-net --unshare-pid --unshare-ipc --unshare-uts --new-session --cap-drop ALL --die-with-parent --clearenv`) for both `exec` and `bg_exec`. With no usable backend, shell tools return an error instead of running on the host. Landlock ABI availability is probed and reported by `soma init`, but Landlock is **not** an enforcement backend yet: without `bwrap`, Linux has no sandbox and shell tools are refused. Pinned verification oracle files are hash-checked before and after every oracle run and bind-mounted read-only (`EROFS`) while the oracle runs; during the agent's own commands they are writable, and tampering is caught by the hash check.
   - **macOS**: Generates deterministic Seatbelt SBPL profiles (`(deny network*)` by default, workspace-confined writes).
   - **Container & Remote Isolation**: Docker and SSH execution backends exist in `internal/sandbox` but are **not yet wired into agent runs**. The Docker virtual desktop (`desktop_task`) is a separate container with its own shell.
2. **Secretless Capability Brokers (`internal/broker`) & Outbound SSRF Guard (`internal/dag`)**:
   - Sandboxed tool executions receive **zero raw API keys, cloud tokens, or SSH private keys**.
   - **Status: the brokers below are implemented and tested, but no production code path constructs a `broker.Router` yet, so the agent's `broker_call` tool currently returns an error.** Privileged external operations (`HTTPBroker`, `SSHBroker`, `FSBroker`) are invoked over host-validated requests where the host enforces domain/path allowlists, command prefix allowlists, remote mutual-exclusion leases (`remote_leases`), and JSON field constraints (such as forcing `{"draft": true}` on GitHub PR creations) before injecting credentials on the host side.
   - `broker.Router` enforces the exact same run-wide untrusted provenance rule as `dag.Executor`: any non-local broker action (`EffectClass != EffectReadOnly && EffectClass != EffectWorkspaceWrite`) automatically unions all `untrusted_external` pages observed by the run into its provenance check, and any `EffectReadOnly` broker response tagged `untrusted_external` is persisted as a provenance `@page`.
   - `web_extract` and `web_search` enforce dial-time and redirect-time SSRF protection (`NewSSRFSafeHTTPClient`, `ValidateSSRFSafeURL`, `IsBlockedSSRFAddr`), blocking loopback (`127.0.0.0/8`, `::1`), RFC-1918 private networks, link-local / cloud metadata (`169.254.169.254`, `100.100.100.200`, `fd00:ec2::254`, `metadata.google.internal`), Carrier-Grade NAT (`100.64.0.0/10`), and DNS-rebound internal addresses.
3. **Data/Action-Level Provenance Taint (`internal/taint`)**:
   - Grounded in *CaMeL* and *MCPTox* defenses against indirect prompt injection and Confused Deputy attacks.
   - Every content-addressed `@page_XXXX` virtual memory blob carries an explicit `trust_tier` (`trusted_user`, `trusted_repo`, or `untrusted_external` for web search, web extraction, and MCP responses).
   - Every untrusted observation is recorded as an `@page`, including ones small enough to stay inline. Reading untrusted content does **not** lock down local `read_only` or sandboxed `workspace_write` exploration, but from then on every `external_write` in that run is treated as referencing all of the run's untrusted pages, whether or not the model cites them, and any `irreversible_write` always asks. Tools the kernel does not recognise (MCP tools, brokers) are classed `external_write`, not `read_only`. Such an action is automatically blocked (`DecisionAsk` -> Operator Inbox) until an operator approves it (approval currently declassifies the cited pages for all capabilities in that run).
   - Outbound reads (`web_search`, `web_extract`, `vision_analyze`, read-only MCP tools) are `external_read`: once the run is tainted, a request whose arguments carry long or encoded data (more than 512 bytes of strings, more than 256 bytes of URL query, or a 40+ character base64/hex token) needs approval, closing the "append the secret to a URL" exfiltration channel. Short ordinary fetches stay autonomous.
   - MCP tools the server marks `destructiveHint: true` are `irreversible_write` and always need approval, even in High Autonomy mode. Server annotations can only tighten a classification, never loosen it.
   - With sandbox networking enabled, output of `exec`/`bg_exec` is tagged `untrusted_external`.
   - A crash while any external or irreversible write was in flight marks it `ambiguous` and suspends the branch for operator reconciliation; it is never retried automatically.
4. **Anti-Poisoning Memory Quarantine (`internal/memory`)**:
   - Observations from `untrusted_external` sources cannot directly activate or supersede bi-temporal memory rules (*MINJA* defense); they are strictly quarantined with `status = 'proposed'` in the Operator Inbox (`soma inbox`). This covers `memory_write` and the durable copy of `publish_finding`.
5. **Skill Immutability (`internal/skills`)**:
   - Agent runs never write `SKILL.md`. `skill_manage create|patch` validates and lints the content, stages it under `.soma/skill_proposals/`, and files a `skill_patch` inbox item; approval applies it and keeps the previous version under `<skill>/.history/`.
   - Project instruction files loaded into the prompt are linted with the same rules; a file matching an injection pattern is replaced by a `[BLOCKED: …]` marker.
6. **Agent-Scheduled Jobs**: `cronjob_manage` can only add or remove triggers it created, which are `notify_only` by default; adding one is an `external_write`, so a tainted run needs approval.

## 2. Known Limitations

- **No seccomp filter or resource limits** on the `bwrap` sandbox yet, and no user-namespace remap: a kernel exploit or a fork bomb inside the sandbox is out of scope today.
- **Taint is tracked per run, not per file.** Untrusted content written into the workspace in one run is read back as `trusted_repo` by a later run.
- **macOS Seatbelt profile is allow-by-default** with targeted denies, which is weaker than the Linux mount-namespace model.
- **Sandbox networking shares the host network namespace.** With "allow outbound internet" on, sandboxed code can reach services on the host's loopback interface, including the dashboard. Keep it off unless needed, and set a dashboard token (`SOMA_WEB_TOKEN`).
- **MCP stdio servers run on the host**, unsandboxed, with the user's environment. A workspace `.soma/mcp.yaml` is loaded automatically: review it before running soma in a repository you did not write.
- **Daemon sentinel commands** (`command`/`stop_command` in `triggers.yaml`) run on the host with a scrubbed environment, not in the sandbox. Only the operator can define them.
- **Secrets in `.soma/harness.json`** (provider keys, bot tokens) are stored in plaintext with mode 0600.

## 3. Reporting a Vulnerability

If you discover a sandbox escape, provenance taint bypass, or broker policy vulnerability, please report it privately via GitHub Security Advisories on [`ra1ncandy/soma`](https://github.com/ra1ncandy/soma/security/advisories).
