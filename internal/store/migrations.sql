PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- 1. Top-level Tasks / Runs
CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    workspace_path TEXT NOT NULL,
    workspace_kind TEXT NOT NULL DEFAULT 'generic',
    goal_spec TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 2. Speculative / Active Execution Branches (Isolated Workspaces)
CREATE TABLE IF NOT EXISTS branches (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    parent_branch_id TEXT REFERENCES branches(id),
    forked_from_step_id TEXT,
    label TEXT NOT NULL,
    head_step_id TEXT,
    epoch INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'queued',
    workspace_dir TEXT NOT NULL DEFAULT '',
    workspace_strategy TEXT NOT NULL DEFAULT 'copy_snapshot',
    active_account_id TEXT,
    stall_count INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 3. Frozen Epoch Prefix Snapshots (Stored Once Per Epoch)
CREATE TABLE IF NOT EXISTS epoch_snapshots (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    branch_id TEXT NOT NULL REFERENCES branches(id) ON DELETE CASCADE,
    epoch INTEGER NOT NULL,
    layer0_blob_hash TEXT NOT NULL,
    layer1_blob_hash TEXT NOT NULL,
    prefix_hash_l0_l1 TEXT NOT NULL,
    metadata_json TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

-- 4. Virtual Memory (@page) Index (Blobs stored zstd-compressed on disk)
CREATE TABLE IF NOT EXISTS pages (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    content_hash TEXT NOT NULL,
    blob_path TEXT NOT NULL,
    content_type TEXT NOT NULL,
    trust_tier TEXT NOT NULL DEFAULT 'trusted_repo',
    declassified INTEGER NOT NULL DEFAULT 0,
    declassified_by TEXT,
    byte_size INTEGER NOT NULL,
    line_count INTEGER NOT NULL,
    preview_text TEXT NOT NULL,
    last_accessed_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

-- 5. Immutable Event-Sourced Execution DAG Steps (True O(T) Incremental Deltas)
CREATE TABLE IF NOT EXISTS steps (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    branch_id TEXT NOT NULL REFERENCES branches(id) ON DELETE CASCADE,
    parent_step_id TEXT REFERENCES steps(id),
    epoch INTEGER NOT NULL,
    turn_index INTEGER NOT NULL,
    account_id TEXT NOT NULL,
    model_id TEXT NOT NULL,
    state_hash TEXT NOT NULL,
    epoch_snapshot_id TEXT NOT NULL REFERENCES epoch_snapshots(id),
    frame_delta_blob_hash TEXT NOT NULL,
    compiled_frame_hash TEXT NOT NULL,
    emitted_blob_hash TEXT NOT NULL,
    observation_page_id TEXT REFERENCES pages(id),
    input_provenance_json TEXT NOT NULL DEFAULT '[]',
    input_tokens INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL DEFAULT 0.0,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

-- 6. 2-Phase Durable Action Journal (Crash Recovery & Idempotency)
CREATE TABLE IF NOT EXISTS action_journal (
    action_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    branch_id TEXT NOT NULL REFERENCES branches(id) ON DELETE CASCADE,
    parent_step_id TEXT REFERENCES steps(id),
    epoch INTEGER NOT NULL,
    turn_index INTEGER NOT NULL,
    tool_name TEXT NOT NULL,
    args_json TEXT NOT NULL,
    args_hash TEXT NOT NULL,
    effect_class TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    input_provenance_json TEXT NOT NULL DEFAULT '[]',
    status TEXT NOT NULL,
    result_page_id TEXT REFERENCES pages(id),
    result_summary TEXT,
    error_text TEXT,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 7. Scoped Provenance Declassifications & Remote Resource Leases
CREATE TABLE IF NOT EXISTS declassifications (
    id TEXT PRIMARY KEY,
    page_id TEXT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    capability_scope TEXT NOT NULL,
    method TEXT NOT NULL,
    approved_by TEXT NOT NULL,
    schema_name TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS remote_leases (
    resource_key TEXT PRIMARY KEY,
    branch_id TEXT NOT NULL REFERENCES branches(id) ON DELETE CASCADE,
    remote_namespace TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);

-- 8. Bi-Temporal Provenance Memory Graph & Negative Paths (Graveyard)
CREATE TABLE IF NOT EXISTS memory_nodes (
    id TEXT PRIMARY KEY,
    scope TEXT NOT NULL,
    kind TEXT NOT NULL,
    content TEXT NOT NULL,
    signature_hash TEXT,
    status TEXT NOT NULL DEFAULT 'active',
    trust_tier TEXT NOT NULL DEFAULT 'trusted_user',
    provenance_step_id TEXT REFERENCES steps(id),
    evidence_page_id TEXT REFERENCES pages(id),
    valid_from INTEGER NOT NULL,
    valid_to INTEGER,
    ingested_at INTEGER NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS memory_edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    from_node_id TEXT NOT NULL REFERENCES memory_nodes(id) ON DELETE CASCADE,
    to_node_id TEXT NOT NULL REFERENCES memory_nodes(id) ON DELETE CASCADE,
    relation TEXT NOT NULL,
    provenance_step_id TEXT REFERENCES steps(id),
    created_at INTEGER NOT NULL,
    UNIQUE(from_node_id, to_node_id, relation)
);

-- 9. Pluggable External Verification Contracts
CREATE TABLE IF NOT EXISTS contracts (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    spec_json TEXT NOT NULL,
    pinned_hash TEXT NOT NULL,
    required INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS contract_runs (
    id TEXT PRIMARY KEY,
    contract_id TEXT NOT NULL REFERENCES contracts(id) ON DELETE CASCADE,
    branch_id TEXT NOT NULL REFERENCES branches(id) ON DELETE CASCADE,
    step_id TEXT NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
    passed INTEGER NOT NULL,
    output_page_id TEXT REFERENCES pages(id),
    artifact_sha256 TEXT,
    counterexample_text TEXT,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

-- 10. Unified Metered & Subscription Account Quota Ledger
CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    model_id TEXT NOT NULL,
    billing_mode TEXT NOT NULL,
    max_concurrent INTEGER NOT NULL DEFAULT 1,
    active_leases INTEGER NOT NULL DEFAULT 0,
    cooldown_until INTEGER NOT NULL DEFAULT 0,
    daily_budget_usd REAL NOT NULL DEFAULT 0.0,
    daily_spent_usd REAL NOT NULL DEFAULT 0.0,
    last_error TEXT,
    updated_at INTEGER NOT NULL
);

-- 11. Tier-0 Event Daemon Runs & Unified Operator Inbox
CREATE TABLE IF NOT EXISTS trigger_runs (
    idempotency_key TEXT PRIMARY KEY,
    trigger_id TEXT NOT NULL,
    last_sentinel_hash TEXT,
    branch_id TEXT REFERENCES branches(id) ON DELETE SET NULL,
    status TEXT NOT NULL,
    cost_usd REAL NOT NULL DEFAULT 0.0,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS inbox_items (
    id TEXT PRIMARY KEY,
    run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
    branch_id TEXT REFERENCES branches(id) ON DELETE SET NULL,
    trigger_id TEXT,
    item_kind TEXT NOT NULL,
    title TEXT NOT NULL,
    summary TEXT NOT NULL,
    artifact_ref TEXT,
    urgency TEXT NOT NULL DEFAULT 'normal',
    status TEXT NOT NULL DEFAULT 'pending',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_steps_branch_turn ON steps(branch_id, epoch, turn_index);
CREATE INDEX IF NOT EXISTS idx_pages_run ON pages(run_id, created_at);
CREATE INDEX IF NOT EXISTS idx_action_journal_branch_status ON action_journal(branch_id, status);
CREATE INDEX IF NOT EXISTS idx_declass_page_cap ON declassifications(page_id, capability_scope);
CREATE INDEX IF NOT EXISTS idx_memory_active ON memory_nodes(scope, status, valid_to);
CREATE INDEX IF NOT EXISTS idx_trigger_expires ON trigger_runs(expires_at);
CREATE INDEX IF NOT EXISTS idx_inbox_pending ON inbox_items(status, created_at);

-- Foreign-key columns: without these, deleting a run makes SQLite scan each child table once per
-- deleted parent row to enforce the constraint (quadratic in run size).
CREATE INDEX IF NOT EXISTS idx_branches_run ON branches(run_id);
CREATE INDEX IF NOT EXISTS idx_branches_parent ON branches(parent_branch_id);
CREATE INDEX IF NOT EXISTS idx_steps_run ON steps(run_id);
CREATE INDEX IF NOT EXISTS idx_steps_parent ON steps(parent_step_id);
CREATE INDEX IF NOT EXISTS idx_steps_obs_page ON steps(observation_page_id);
CREATE INDEX IF NOT EXISTS idx_steps_snapshot ON steps(epoch_snapshot_id);
CREATE INDEX IF NOT EXISTS idx_epoch_snapshots_run ON epoch_snapshots(run_id);
CREATE INDEX IF NOT EXISTS idx_epoch_snapshots_branch ON epoch_snapshots(branch_id, epoch);
CREATE INDEX IF NOT EXISTS idx_action_journal_run ON action_journal(run_id);
CREATE INDEX IF NOT EXISTS idx_action_journal_parent ON action_journal(parent_step_id);
CREATE INDEX IF NOT EXISTS idx_action_journal_page ON action_journal(result_page_id);
CREATE INDEX IF NOT EXISTS idx_memory_prov_step ON memory_nodes(provenance_step_id);
CREATE INDEX IF NOT EXISTS idx_memory_evidence_page ON memory_nodes(evidence_page_id);
CREATE INDEX IF NOT EXISTS idx_memory_edges_to ON memory_edges(to_node_id);
CREATE INDEX IF NOT EXISTS idx_memory_edges_prov ON memory_edges(provenance_step_id);
CREATE INDEX IF NOT EXISTS idx_contracts_run ON contracts(run_id);
CREATE INDEX IF NOT EXISTS idx_contract_runs_branch ON contract_runs(branch_id);
CREATE INDEX IF NOT EXISTS idx_contract_runs_step ON contract_runs(step_id);
CREATE INDEX IF NOT EXISTS idx_contract_runs_contract ON contract_runs(contract_id);
CREATE INDEX IF NOT EXISTS idx_contract_runs_page ON contract_runs(output_page_id);
CREATE INDEX IF NOT EXISTS idx_inbox_run ON inbox_items(run_id);
CREATE INDEX IF NOT EXISTS idx_inbox_branch ON inbox_items(branch_id);
CREATE INDEX IF NOT EXISTS idx_trigger_runs_branch ON trigger_runs(branch_id);
CREATE INDEX IF NOT EXISTS idx_trigger_runs_trigger ON trigger_runs(trigger_id, created_at);
CREATE INDEX IF NOT EXISTS idx_remote_leases_branch ON remote_leases(branch_id);
CREATE INDEX IF NOT EXISTS idx_pages_trust ON pages(run_id, trust_tier);

-- 12. Matryoshka Int8 Vector Index (Memory Nodes & @page Chunks)
CREATE TABLE IF NOT EXISTS embeddings (
    kind TEXT NOT NULL,
    target_id TEXT NOT NULL,
    chunk_index INTEGER NOT NULL DEFAULT 0,
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line INTEGER NOT NULL DEFAULT 0,
    model_id TEXT NOT NULL DEFAULT '',
    dims INTEGER NOT NULL,
    vec_int8 BLOB NOT NULL,
    snippet TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    PRIMARY KEY (kind, target_id, chunk_index)
);
-- (kind, target_id) is a prefix of the primary key; a separate index only slowed writes.
DROP INDEX IF EXISTS idx_embeddings_kind_target;
