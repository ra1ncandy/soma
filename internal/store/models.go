package store

// Run represents a top-level task execution unit.
type Run struct {
	ID            string
	Title         string
	WorkspacePath string
	WorkspaceKind string
	GoalSpec      string
	Status        string
	CreatedAt     int64
	UpdatedAt     int64
}

// Branch represents a linear or speculatively forked trajectory with an isolated workspace.
type Branch struct {
	ID                string
	RunID             string
	ParentBranchID    string
	ForkedFromStepID  string
	Label             string
	HeadStepID        string
	Epoch             int
	Status            string
	WorkspaceDir      string
	WorkspaceStrategy string
	ActiveAccountID   string
	StallCount        int
	CreatedAt         int64
	UpdatedAt         int64
}

// EpochSnapshot freezes Layer 0 and Layer 1 blobs once per epoch for linear O(T) storage.
type EpochSnapshot struct {
	ID             string
	RunID          string
	BranchID       string
	Epoch          int
	Layer0BlobHash string
	Layer1BlobHash string
	PrefixHashL0L1 string
	MetadataJSON   string
	CreatedAt      int64
}

// Page indexes a spilled content-addressed observation stored on disk.
type Page struct {
	ID             string
	RunID          string
	ContentHash    string
	BlobPath       string
	ContentType    string
	TrustTier      string
	Declassified   bool
	DeclassifiedBy string
	ByteSize       int64
	LineCount      int
	PreviewText    string
	LastAccessedAt int64
	CreatedAt      int64
}

// Step represents a single immutable content-addressed Merkle DAG step referencing an EpochSnapshot + FrameDelta.
type Step struct {
	ID                  string
	RunID               string
	BranchID            string
	ParentStepID        string
	Epoch               int
	TurnIndex           int
	AccountID           string
	ModelID             string
	StateHash           string
	EpochSnapshotID     string
	FrameDeltaBlobHash  string
	CompiledFrameHash   string
	EmittedBlobHash     string
	ObservationPageID   string
	InputProvenanceJSON string
	InputTokens         int
	CacheReadTokens     int
	CacheWriteTokens    int
	OutputTokens        int
	CostUSD             float64
	LatencyMS           int64
	CreatedAt           int64
}

// ActionJournalEntry tracks two-phase dispatch and idempotency state across crashes.
type ActionJournalEntry struct {
	ActionID            string
	RunID               string
	BranchID            string
	ParentStepID        string
	Epoch               int
	TurnIndex           int
	ToolName            string
	ArgsJSON            string
	ArgsHash            string
	EffectClass         string
	IdempotencyKey      string
	InputProvenanceJSON string
	Status              string
	ResultPageID        string
	ResultSummary       string
	ErrorText           string
	CreatedAt           int64
	UpdatedAt           int64
}

// Declassification authorizes a specific capability_scope to consume an untrusted @page.
type Declassification struct {
	ID              string
	PageID          string
	CapabilityScope string
	Method          string
	ApprovedBy      string
	SchemaName      string
	CreatedAt       int64
}

// RemoteLease coordinates mutual exclusion over shared remote external resources across branches.
type RemoteLease struct {
	ResourceKey     string
	BranchID        string
	RemoteNamespace string
	ExpiresAt       int64
	CreatedAt       int64
}

// MemoryNode represents a bi-temporal rule, fact, preference, or Graveyard negative_path.
type MemoryNode struct {
	ID               string
	Scope            string
	Kind             string
	Content          string
	SignatureHash    string
	Status           string
	TrustTier        string
	ProvenanceStepID string
	EvidencePageID   string
	ValidFrom        int64
	ValidTo          *int64
	IngestedAt       int64
	Enabled          bool
}

// MemoryEdge represents a directed typed relation between two MemoryNodes.
type MemoryEdge struct {
	ID               int64
	FromNodeID       string
	ToNodeID         string
	Relation         string
	ProvenanceStepID string
	CreatedAt        int64
}

// Account tracks metered budget and subscription rolling cooldown/lease state.
type Account struct {
	ID             string
	Provider       string
	ModelID        string
	BillingMode    string
	MaxConcurrent  int
	ActiveLeases   int
	CooldownUntil  int64
	DailyBudgetUSD float64
	DailySpentUSD  float64
	LastError      string
	UpdatedAt      int64
}

// InboxItem represents an actionable operator review, approval, or reconciliation task.
type InboxItem struct {
	ID          string
	RunID       string
	BranchID    string
	TriggerID   string
	ItemKind    string
	Title       string
	Summary     string
	ArtifactRef string
	Urgency     string
	Status      string
	CreatedAt   int64
}
