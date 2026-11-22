package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SyscallDef describes a built-in kernel syscall included in Layer 0.
type SyscallDef struct {
	Name      string `json:"name"`
	Signature string `json:"signature"`
}

// SkillMeta holds startup AAIF SKILL.md frontmatter metadata for Layer 0.
type SkillMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SkillBody holds an activated skill body carried across epochs into Layer 1.
type SkillBody struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

// MCPSummary holds a 1-line epoch-pinned MCP server summary for Layer 0.
type MCPSummary struct {
	Server  string `json:"server"`
	Summary string `json:"summary"`
}

// ToolSchema holds a deferred MCP tool JSON schema loaded via tool_search.
type ToolSchema struct {
	Name       string `json:"name"`
	JSONSchema string `json:"json_schema"`
}

// MemoryEntry holds a scoped active rule, fact, or preference for Layer 1.
type MemoryEntry struct {
	ID      string `json:"id"`
	Scope   string `json:"scope"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// GraveyardEntry holds an immutable failed approach carried in Layer 1 across epochs.
type GraveyardEntry struct {
	ID                string `json:"id"`
	SignatureHash     string `json:"signature_hash"`
	AttemptedApproach string `json:"attempted_approach"`
	FailureReason     string `json:"failure_reason"`
	EvidencePageID    string `json:"evidence_page_id"`
}

// EpochInput holds all static/snapshot inputs used to freeze Layer 0 and Layer 1 for an epoch.
type EpochInput struct {
	SystemPrompt      string
	WorkspaceMode     string
	TaskGoal          string
	ContractSpec      string
	Syscalls          []SyscallDef
	SkillIndex        []SkillMeta
	MCPSummaries      []MCPSummary
	ActiveMemory      []MemoryEntry
	Graveyard         []GraveyardEntry
	CarriedSkills     []SkillBody
	CarriedMCPSchemas []ToolSchema
}

// EpochSnapshot is strictly immutable for the duration of an epoch.
type EpochSnapshot struct {
	Epoch             int          `json:"epoch"`
	Layer0Text        string       `json:"-"`
	Layer0Hash        string       `json:"layer0_hash"`
	Layer1Text        string       `json:"-"`
	Layer1Hash        string       `json:"layer1_hash"`
	PrefixHashL0L1    string       `json:"prefix_hash_l0_l1"`
	CarriedSkills     []SkillBody  `json:"carried_skills,omitempty"`
	CarriedMCPSchemas []ToolSchema `json:"carried_mcp_schemas,omitempty"`
}

// LogEntry represents an append-only item in Layer 2 (Working Log).
// Mid-epoch skill activations, deferred MCP schemas, and notices MUST append here, never to Layer 1.
type LogEntry struct {
	Kind        string `json:"kind"` // turn | dynamic_skill | dynamic_mcp | rule_notice | graveyard_notice | slice_result
	TurnIndex   int    `json:"turn_index"`
	Name        string `json:"name,omitempty"`
	Action      string `json:"action,omitempty"`
	Reasoning   string `json:"reasoning,omitempty"`
	Observation string `json:"observation,omitempty"`
	PageID      string `json:"page_id,omitempty"`
	Masked      bool   `json:"masked,omitempty"`
}

// LiveTailState represents uncached Layer 3 telemetry rebuilt every turn.
type LiveTailState struct {
	WorkspaceKind       string `json:"workspace_kind"`
	GitDiffStat         string `json:"git_diff_stat,omitempty"`
	ResearchStatus      string `json:"research_status,omitempty"`
	LastOracleError     string `json:"last_oracle_error,omitempty"`
	StepBudgetRemaining int    `json:"step_budget_remaining"`
	AccountQuotaSummary string `json:"account_quota_summary,omitempty"`
	StallCount          int    `json:"stall_count"`
	MaxStalls           int    `json:"max_stalls"`
}

// FrameDelta is the incremental per-turn delta persisted to BlobStore for O(T) storage.
type FrameDelta struct {
	TurnIndex  int           `json:"turn_index"`
	AppendedL2 []LogEntry    `json:"appended_l2,omitempty"`
	LiveTail   LiveTailState `json:"live_tail"`
}

// CacheBreakpoint marks a provider cache boundary inside the compiled frame.
type CacheBreakpoint struct {
	Layer       string `json:"layer"`
	OffsetBytes int    `json:"offset_bytes"`
	TTL         string `json:"ttl"`
}

// CompiledFrame holds the complete 4-layer prompt payload, breakpoints, and hashes.
type CompiledFrame struct {
	Epoch             int               `json:"epoch"`
	Layer0            string            `json:"layer0"`
	Layer1            string            `json:"layer1"`
	Layer2            string            `json:"layer2"`
	Layer3            string            `json:"layer3"`
	Breakpoints       []CacheBreakpoint `json:"breakpoints"`
	PrefixHashL0L1    string            `json:"-"`
	CompiledFrameHash string            `json:"-"`
	L0Tokens          int               `json:"-"`
	L1Tokens          int               `json:"-"`
	L2Tokens          int               `json:"-"`
	L3Tokens          int               `json:"-"`
	TotalTokens       int               `json:"-"`
}

type canonicalWireFrame struct {
	Epoch       int               `json:"epoch"`
	Layer0      string            `json:"layer0"`
	Layer1      string            `json:"layer1"`
	Layer2      string            `json:"layer2"`
	Layer3      string            `json:"layer3"`
	Breakpoints []CacheBreakpoint `json:"breakpoints"`
}

// WireBytes returns the canonical deterministic JSON bytes sent over the wire.
func (f CompiledFrame) WireBytes() []byte {
	w := canonicalWireFrame{
		Epoch:       f.Epoch,
		Layer0:      f.Layer0,
		Layer1:      f.Layer1,
		Layer2:      f.Layer2,
		Layer3:      f.Layer3,
		Breakpoints: f.Breakpoints,
	}
	b, _ := json.Marshal(w)
	return b
}

// DefaultCoreSyscalls returns the canonically sorted core v2.1 syscalls.
func DefaultCoreSyscalls() []SyscallDef {
	return []SyscallDef{
		{Name: "branch_fork", Signature: "branch_fork(label, reason)"},
		{Name: "broker_call", Signature: "broker_call(capability, op, params_json)"},
		{Name: "exec", Signature: "exec(cmd, args_json)"},
		{Name: "fs_patch", Signature: "fs_patch(path, unified_diff)"},
		{Name: "fs_read", Signature: "fs_read(path)"},
		{Name: "memory_search", Signature: "memory_search(query, scope?)"},
		{Name: "page_grep", Signature: "page_grep(page_id, pattern)"},
		{Name: "page_search", Signature: "page_search(query, page_id?)"},
		{Name: "page_slice", Signature: "page_slice(page_id, start_line, end_line)"},
		{Name: "tool_search", Signature: "tool_search(query)"},
	}
}

// NewEpochSnapshot builds a deterministically sorted, frozen Layer 0 + Layer 1 snapshot.
func NewEpochSnapshot(epoch int, in EpochInput) EpochSnapshot {
	syscalls := append([]SyscallDef(nil), in.Syscalls...)
	if len(syscalls) == 0 {
		syscalls = DefaultCoreSyscalls()
	}
	sort.Slice(syscalls, func(i, j int) bool { return syscalls[i].Name < syscalls[j].Name })

	skills := append([]SkillMeta(nil), in.SkillIndex...)
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })

	mcps := append([]MCPSummary(nil), in.MCPSummaries...)
	sort.Slice(mcps, func(i, j int) bool { return mcps[i].Server < mcps[j].Server })

	var l0 strings.Builder
	fmt.Fprintf(&l0, "=== SOMA KERNEL v2.1 EPOCH %d [LAYER 0: FROZEN] ===\n", epoch)
	if in.SystemPrompt != "" {
		l0.WriteString(strings.TrimSpace(in.SystemPrompt))
		l0.WriteByte('\n')
	}
	if in.WorkspaceMode != "" {
		fmt.Fprintf(&l0, "workspace_mode: %s\n", in.WorkspaceMode)
	}
	l0.WriteString("syscalls:\n")
	for _, sc := range syscalls {
		fmt.Fprintf(&l0, "  - %s: %s\n", sc.Name, sc.Signature)
	}
	if len(skills) > 0 {
		l0.WriteString("skill_index:\n")
		for _, sk := range skills {
			fmt.Fprintf(&l0, "  - %s: %s\n", sk.Name, sk.Description)
		}
	}
	if len(mcps) > 0 {
		l0.WriteString("mcp_servers:\n")
		for _, m := range mcps {
			fmt.Fprintf(&l0, "  - %s: %s\n", m.Server, m.Summary)
		}
	}
	fmt.Fprintf(&l0, "task_goal: %s\n", strings.TrimSpace(in.TaskGoal))
	if in.ContractSpec != "" {
		fmt.Fprintf(&l0, "verification_contract: %s\n", strings.TrimSpace(in.ContractSpec))
	}

	mem := append([]MemoryEntry(nil), in.ActiveMemory...)
	sort.Slice(mem, func(i, j int) bool { return mem[i].ID < mem[j].ID })

	grave := append([]GraveyardEntry(nil), in.Graveyard...)
	sort.Slice(grave, func(i, j int) bool { return grave[i].ID < grave[j].ID })

	carriedSkills := append([]SkillBody(nil), in.CarriedSkills...)
	sort.Slice(carriedSkills, func(i, j int) bool { return carriedSkills[i].Name < carriedSkills[j].Name })

	carriedMCP := append([]ToolSchema(nil), in.CarriedMCPSchemas...)
	sort.Slice(carriedMCP, func(i, j int) bool { return carriedMCP[i].Name < carriedMCP[j].Name })

	var l1 strings.Builder
	l1.WriteString("=== [LAYER 1: FROZEN EPOCH REGISTERS] ===\n")
	l1.WriteString("active_rules:\n")
	for _, m := range mem {
		fmt.Fprintf(&l1, "  [%s] (%s/%s): %s\n", m.ID, m.Scope, m.Kind, m.Content)
	}
	l1.WriteString("graveyard_negative_paths:\n")
	for _, g := range grave {
		fmt.Fprintf(&l1, "  [%s] sig=%s approach=%q reason=%q evidence=%s\n",
			g.ID, g.SignatureHash, g.AttemptedApproach, g.FailureReason, g.EvidencePageID)
	}
	if len(carriedSkills) > 0 {
		l1.WriteString("activated_skills:\n")
		for _, sk := range carriedSkills {
			fmt.Fprintf(&l1, "  <skill name=%q>\n%s\n  </skill>\n", sk.Name, strings.TrimSpace(sk.Body))
		}
	}
	if len(carriedMCP) > 0 {
		l1.WriteString("activated_mcp_schemas:\n")
		for _, ts := range carriedMCP {
			fmt.Fprintf(&l1, "  <tool name=%q>%s</tool>\n", ts.Name, strings.TrimSpace(ts.JSONSchema))
		}
	}

	l0Str := l0.String()
	l1Str := l1.String()
	l0Hash := sha256Hex([]byte(l0Str))
	l1Hash := sha256Hex([]byte(l1Str))
	prefixHash := sha256Hex([]byte(l0Str + "\n" + l1Str))

	return EpochSnapshot{
		Epoch:             epoch,
		Layer0Text:        l0Str,
		Layer0Hash:        l0Hash,
		Layer1Text:        l1Str,
		Layer1Hash:        l1Hash,
		PrefixHashL0L1:    prefixHash,
		CarriedSkills:     carriedSkills,
		CarriedMCPSchemas: carriedMCP,
	}
}

// Compile assembles the 4-layer prompt frame, enforcing strict monotonicity of Layers 0-2.
func Compile(snap EpochSnapshot, l2 []LogEntry, tail LiveTailState, profile ModelProfile) CompiledFrame {
	var l2Buf strings.Builder
	l2Buf.WriteString("=== [LAYER 2: APPEND-ONLY WORKING LOG] ===\n")
	for _, entry := range l2 {
		l2Buf.WriteString(formatLogEntry(entry))
		l2Buf.WriteByte('\n')
	}

	var l3Buf strings.Builder
	l3Buf.WriteString("=== [LAYER 3: EPHEMERAL LIVE TAIL] ===\n")
	fmt.Fprintf(&l3Buf, "workspace_kind: %s\n", tail.WorkspaceKind)
	if tail.GitDiffStat != "" {
		fmt.Fprintf(&l3Buf, "git_diff_stat:\n%s\n", strings.TrimSpace(tail.GitDiffStat))
	}
	if tail.ResearchStatus != "" {
		fmt.Fprintf(&l3Buf, "workspace_status: %s\n", strings.TrimSpace(tail.ResearchStatus))
	}
	if tail.LastOracleError != "" {
		fmt.Fprintf(&l3Buf, "last_oracle_error: %s\n", strings.TrimSpace(tail.LastOracleError))
	}
	fmt.Fprintf(&l3Buf, "budget_remaining: %d | stalls: %d/%d | quota: %s\n",
		tail.StepBudgetRemaining, tail.StallCount, tail.MaxStalls, tail.AccountQuotaSummary)

	l0 := snap.Layer0Text
	l1 := snap.Layer1Text
	l2Str := l2Buf.String()
	l3Str := l3Buf.String()

	bps := buildBreakpoints(profile.ProviderFlavor, len(l0), len(l1), len(l2Str))
	frame := CompiledFrame{
		Epoch:          snap.Epoch,
		Layer0:         l0,
		Layer1:         l1,
		Layer2:         l2Str,
		Layer3:         l3Str,
		Breakpoints:    bps,
		PrefixHashL0L1: snap.PrefixHashL0L1,
		L0Tokens:       EstimateTokens(l0),
		L1Tokens:       EstimateTokens(l1),
		L2Tokens:       EstimateTokens(l2Str),
		L3Tokens:       EstimateTokens(l3Str),
	}
	frame.TotalTokens = frame.L0Tokens + frame.L1Tokens + frame.L2Tokens + frame.L3Tokens
	frame.CompiledFrameHash = sha256Hex(frame.WireBytes())
	return frame
}

func formatLogEntry(e LogEntry) string {
	switch e.Kind {
	case "dynamic_skill":
		return fmt.Sprintf("[t%d:skill_load name=%q]\n%s", e.TurnIndex, e.Name, strings.TrimSpace(e.Observation))
	case "dynamic_mcp":
		return fmt.Sprintf("[t%d:mcp_schema tool=%q] %s", e.TurnIndex, e.Name, strings.TrimSpace(e.Observation))
	case "rule_notice":
		return fmt.Sprintf("[t%d:rule_notice id=%q] %s", e.TurnIndex, e.Name, strings.TrimSpace(e.Observation))
	case "graveyard_notice":
		return fmt.Sprintf("[t%d:graveyard_notice id=%q] %s", e.TurnIndex, e.Name, strings.TrimSpace(e.Observation))
	case "handoff":
		return fmt.Sprintf("[t%d:handoff account=%q]\n%s", e.TurnIndex, e.Name, strings.TrimSpace(e.Observation))
	case "slice_result":
		if e.Masked {
			return fmt.Sprintf("[t%d:slice_masked page=%s]", e.TurnIndex, e.PageID)
		}
		return fmt.Sprintf("[t%d:slice page=%s]\n%s", e.TurnIndex, e.PageID, strings.TrimSpace(e.Observation))
	default:
		if e.Masked {
			obsRef := e.Observation
			if e.PageID != "" {
				obsRef = "@" + e.PageID + " (masked at rollover)"
			}
			return fmt.Sprintf("[t%d] thought=%q action=%q obs=%s", e.TurnIndex, e.Reasoning, e.Action, obsRef)
		}
		return fmt.Sprintf("[t%d] thought=%q action=%q\nobs:\n%s", e.TurnIndex, e.Reasoning, e.Action, strings.TrimSpace(e.Observation))
	}
}

func buildBreakpoints(flavor string, l0Len, l1Len, l2Len int) []CacheBreakpoint {
	ttl0, ttl1, ttl2 := "30m", "30m", "30m"
	switch flavor {
	case "anthropic":
		// Longest TTL checkpoints first: 1h on static L0/L1, 5m rolling on L2.
		ttl0, ttl1, ttl2 = "1h", "1h", "5m"
	case "local":
		ttl0, ttl1, ttl2 = "radix", "radix", "radix"
	}
	return []CacheBreakpoint{
		{Layer: "L0", OffsetBytes: l0Len, TTL: ttl0},
		{Layer: "L1", OffsetBytes: l0Len + l1Len, TTL: ttl1},
		{Layer: "L2", OffsetBytes: l0Len + l1Len + l2Len, TTL: ttl2},
	}
}

// NeedsRollover reports whether the frame has reached the profile's epoch ceiling.
func NeedsRollover(frame CompiledFrame, profile ModelProfile) bool {
	return profile.EpochTokenCeiling > 0 && frame.TotalTokens >= profile.EpochTokenCeiling
}

// Rollover promotes Layer-2 dynamic skills/MCP schemas into Epoch N+1 Layer 1 and masks verbose
// observations while retaining compact reasoning/action skeletons to prevent uncommitted-work amnesia.
func Rollover(
	prev EpochSnapshot,
	l2 []LogEntry,
	activeMem []MemoryEntry,
	graveyard []GraveyardEntry,
	baseIn EpochInput,
) (EpochSnapshot, []LogEntry) {
	skillMap := make(map[string]SkillBody, len(prev.CarriedSkills))
	for _, s := range prev.CarriedSkills {
		skillMap[s.Name] = s
	}
	mcpMap := make(map[string]ToolSchema, len(prev.CarriedMCPSchemas))
	for _, m := range prev.CarriedMCPSchemas {
		mcpMap[m.Name] = m
	}

	var skeleton []LogEntry
	for _, e := range l2 {
		switch e.Kind {
		case "dynamic_skill":
			skillMap[e.Name] = SkillBody{Name: e.Name, Body: e.Observation}
		case "dynamic_mcp":
			mcpMap[e.Name] = ToolSchema{Name: e.Name, JSONSchema: e.Observation}
		case "rule_notice", "graveyard_notice":
			// Folded into refreshed Layer 1 registers on the next epoch.
		case "slice_result":
			// Masked to compact pointer if page_id exists.
			if e.PageID != "" {
				skeleton = append(skeleton, LogEntry{
					Kind:      "slice_result",
					TurnIndex: e.TurnIndex,
					PageID:    e.PageID,
					Masked:    true,
				})
			}
		default:
			maskedObs := e.Observation
			if e.PageID == "" && len(maskedObs) > 160 {
				maskedObs = maskedObs[:157] + "..."
			}
			skeleton = append(skeleton, LogEntry{
				Kind:        "turn",
				TurnIndex:   e.TurnIndex,
				Name:        e.Name,
				Action:      e.Action,
				Reasoning:   e.Reasoning,
				Observation: maskedObs,
				PageID:      e.PageID,
				Masked:      true,
			})
		}
	}

	var carriedSkills []SkillBody
	for _, s := range skillMap {
		carriedSkills = append(carriedSkills, s)
	}
	var carriedMCP []ToolSchema
	for _, m := range mcpMap {
		carriedMCP = append(carriedMCP, m)
	}

	baseIn.ActiveMemory = activeMem
	baseIn.Graveyard = graveyard
	baseIn.CarriedSkills = carriedSkills
	baseIn.CarriedMCPSchemas = carriedMCP

	nextSnap := NewEpochSnapshot(prev.Epoch+1, baseIn)
	return nextSnap, skeleton
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
