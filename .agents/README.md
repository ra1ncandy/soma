# `.agents/` — AI Development Configuration for `soma`

This directory holds project-specific rules, skills, and architectural references for AI coding agents working on **`soma`** (compatible with Antigravity and the AAIF Agent Skills specification).

## Directory Layout

```text
.agents/
├── README.md                          # This index
├── references/
│   └── architecture.md                # Package-by-package map of cmd/, internal/, benchmarks/
├── rules/                             # Always-on engineering constraints
│   ├── kernel-invariants.md           # Pure Go (CGO_ENABLED=0), 4-layer compiler, 2-phase journal, SQLite WAL
│   ├── security-and-sandbox.md        # Fail-closed bwrap/Seatbelt, run-wide taint, SSRF guard, MINJA quarantine
│   └── benchmarks-and-honesty.md      # Conformance vs. live eval separation, reproducible claims
└── skills/                            # On-demand procedural runbooks (AAIF SKILL.md format)
    ├── verify-and-build/SKILL.md      # Local & CI vet, race tests, static builds, shadow report
    ├── compiler-and-replay/SKILL.md   # 4-layer prompt compiler, @page vmem, ModeExact SHA-256 replay
    ├── dag-and-swarm/SKILL.md         # DAG executor, tools, 2-phase journal, branching, swarm & 3-way merge
    ├── security-taint-sandbox/SKILL.md# Sandbox backends, provenance taint, brokers, SSRF & B3/B4/B5 gates
    └── live-eval-benchmarks/SKILL.md  # Live LLM A/B eval & Hard Gauntlet (offline pins + live runs)
```

## Authoring New Skills

When adding a skill under `.agents/skills/<skill-name>/SKILL.md`:

1. **Folder & Name Parity**: `<skill-name>` must match `^[a-z0-9]+(-[a-z0-9]+)*$` (1–64 chars) and match the `name:` field in YAML frontmatter.
2. **Concise Frontmatter**: Include `name` and a clear third-person `description` (1–1024 chars) stating what the skill does and when to activate it.
3. **Progressive Disclosure**: Keep `SKILL.md` focused on concrete procedures and verification steps; place bulky reference material under `references/` or `.agents/references/`.
4. **Lint Compatibility**: Keep `SKILL.md` and any `scripts/` clean of hidden Unicode control characters or patterns flagged by `internal/skills/lint.go`.
