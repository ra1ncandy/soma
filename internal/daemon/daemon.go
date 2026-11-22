package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
)

// TriggerOutcome reports the deterministic result of evaluating or firing a Tier-0 trigger.
type TriggerOutcome struct {
	TriggerID      string `json:"trigger_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Status         string `json:"status"` // skipped | coalesced | inhibited | stopped | done | failed
	Reason         string `json:"reason,omitempty"`
	BranchID       string `json:"branch_id,omitempty"`
	SentinelHash   string `json:"sentinel_hash,omitempty"`
	CostUSD        float64 `json:"cost_usd,omitempty"`
}

// CommandRunner executes sentinel/watch shell probes inside workspaceDir.
type CommandRunner func(ctx context.Context, command, workspaceDir string) (stdout []byte, exitCode int, err error)

// TriggerHandler executes a claimed, non-inhibited coalesced trigger event.
// warmBranchID is non-empty when session_mode=="warm_branch" has an unexpired branch.
type TriggerHandler func(ctx context.Context, spec TriggerSpec, ev CoalescedEvent, warmBranchID string) (branchID string, costUSD float64, err error)

type warmBranchRef struct {
	branchID  string
	expiresAt time.Time
}

// Daemon implements soma's zero-token Tier-0 event pipeline, coalescer, inhibitory brakes, and TTL GC.
type Daemon struct {
	Store         *store.Store
	Config        Config
	WorkspaceDir  string
	Provider      dag.ModelProvider
	Profile       compiler.ModelProfile
	CommandRunner CommandRunner
	Handler       TriggerHandler
	Coalescer     *Coalescer
	Now           func() time.Time

	SkippedZeroToken atomic.Int64
	CoalescedCount   atomic.Int64
	DispatchedCount  atomic.Int64
	InhibitedCount   atomic.Int64

	mu           sync.Mutex
	warmBranches map[string]warmBranchRef
	sentinelSeq  atomic.Uint64
	listenAddr   string
	runCtx       context.Context
}

// New constructs a Daemon configured for st and cfg.
func New(st *store.Store, cfg Config, workspaceDir string) *Daemon {
	if cfg.MaxPendingInbox == 0 && len(cfg.Triggers) == 0 && cfg.WebhookAddr == "" {
		cfg = DefaultConfig()
	}
	if cfg.WebhookAddr == "" {
		cfg.WebhookAddr = "127.0.0.1:8787"
	}
	if cfg.GCInterval == "" {
		cfg.GCInterval = "5m"
	}
	d := &Daemon{
		Store:        st,
		Config:       cfg,
		WorkspaceDir: workspaceDir,
		Profile:      compiler.WorkerProfile(),
		Coalescer:    NewCoalescer(),
		Now:          time.Now,
		warmBranches: make(map[string]warmBranchRef),
	}
	d.CommandRunner = d.defaultCommandRunner
	d.Handler = d.defaultTriggerHandler
	return d
}

// BoundWebhookAddr returns the active TCP listen address after Start binds the webhook listener.
func (d *Daemon) BoundWebhookAddr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listenAddr
}

func (d *Daemon) effectiveCtx() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.runCtx != nil {
		return d.runCtx
	}
	return context.Background()
}

func (d *Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Daemon) defaultCommandRunner(ctx context.Context, command, workspaceDir string) ([]byte, int, error) {
	if strings.TrimSpace(command) == "" {
		return nil, 0, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	if workspaceDir != "" {
		cmd.Dir = workspaceDir
	}
	cmd.Env = sandbox.ScrubEnv(nil)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return out, exitErr.ExitCode(), err
		}
		return out, -1, err
	}
	return out, 0, nil
}

func (d *Daemon) defaultTriggerHandler(ctx context.Context, spec TriggerSpec, ev CoalescedEvent, warmBranchID string) (string, float64, error) {
	goal := strings.TrimSpace(spec.Prompt)
	if goal == "" {
		goal = fmt.Sprintf("Tier-0 trigger %s (%s)", spec.ID, spec.Type)
	}
	if spec.Capability == "notify_only" || d.Provider == nil {
		urgency := spec.Urgency
		if urgency == "" {
			urgency = "normal"
		}
		summary := fmt.Sprintf("Trigger=%s source=%s coalesced=%d key=%s\n%s",
			spec.ID, ev.Source, ev.Count, ev.IdempotencyKey, truncateStr(string(ev.Payload), 500))
		if err := d.Store.InsertInboxItem(ctx, store.InboxItem{
			ID:        fmt.Sprintf("inb_trig_%d_%s", d.now().UnixNano(), spec.ID),
			TriggerID: spec.ID,
			ItemKind:  "draft_pr",
			Title:     fmt.Sprintf("[%s] %s", spec.ID, goal),
			Summary:   summary,
			Urgency:   urgency,
		}); err != nil {
			return "", 0, err
		}
		return "", 0, nil
	}

	maxTurns := spec.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 12
	}
	exec := dag.NewExecutor(d.Store, d.Provider, nil, d.Profile)
	branchID := warmBranchID
	if branchID == "" {
		var err error
		_, branchID, err = exec.StartRun(ctx, dag.RunRequest{
			Title:         fmt.Sprintf("trigger:%s", spec.ID),
			WorkspacePath: d.WorkspaceDir,
			GoalSpec:      fmt.Sprintf("%s\n\nEvent payload:\n%s", goal, truncateStr(string(ev.Payload), 1200)),
			Profile:       d.Profile,
		})
		if err != nil {
			return "", 0, err
		}
	}
	if _, err := exec.ExecuteBranch(ctx, branchID, maxTurns); err != nil {
		return branchID, 0, err
	}
	return branchID, 0, nil
}

// EvaluateSentinel runs a zero-token command/watch probe and only invokes Handler on configured changes.
func (d *Daemon) EvaluateSentinel(ctx context.Context, spec TriggerSpec) (TriggerOutcome, error) {
	if strings.TrimSpace(spec.StopCommand) != "" {
		_, code, stopErr := d.CommandRunner(ctx, spec.StopCommand, d.WorkspaceDir)
		if stopErr == nil && code == 0 {
			return TriggerOutcome{
				TriggerID: spec.ID,
				Status:    "stopped",
				Reason:    "stop_command succeeded",
			}, nil
		}
	}

	stdout, exitCode, cmdErr := d.CommandRunner(ctx, spec.Command, d.WorkspaceDir)
	sum := sha256Hex(stdout)
	wakeOn := strings.TrimSpace(spec.WakeOn)
	if wakeOn == "" {
		wakeOn = "output_hash_changed"
	}

	if wakeOn == "nonzero" && cmdErr == nil && exitCode == 0 {
		d.SkippedZeroToken.Add(1)
		return TriggerOutcome{
			TriggerID:    spec.ID,
			Status:       "skipped",
			SentinelHash: sum,
			Reason:       "exit_code=0",
		}, nil
	}

	if wakeOn == "output_hash_changed" {
		if cmdErr != nil && len(stdout) == 0 {
			return TriggerOutcome{
				TriggerID: spec.ID,
				Status:    "failed",
				Reason:    fmt.Sprintf("sentinel command failed: %v", cmdErr),
			}, cmdErr
		}
		lastHash, err := d.Store.GetLatestTriggerHash(ctx, spec.ID)
		if err != nil {
			return TriggerOutcome{}, err
		}
		if lastHash == "" {
			now := d.now()
			ret := effectiveRetention(spec)
			_, _, err := d.Store.TryClaimTriggerRun(ctx, store.TriggerRun{
				IdempotencyKey:   fmt.Sprintf("base_%s_%s", spec.ID, sum[:12]),
				TriggerID:        spec.ID,
				LastSentinelHash: sum,
				Status:           "skipped",
				CreatedAt:        now.Unix(),
				ExpiresAt:        now.Add(ret).Unix(),
			})
			if err != nil {
				return TriggerOutcome{}, err
			}
			d.SkippedZeroToken.Add(1)
			return TriggerOutcome{
				TriggerID:    spec.ID,
				Status:       "skipped",
				SentinelHash: sum,
				Reason:       "baseline_recorded",
			}, nil
		}
		if sum == lastHash {
			d.SkippedZeroToken.Add(1)
			return TriggerOutcome{
				TriggerID:    spec.ID,
				Status:       "skipped",
				SentinelHash: sum,
				Reason:       "unchanged_hash",
			}, nil
		}
	}

	now := d.now()
	chgSeq := d.sentinelSeq.Add(1)
	ev := Event{
		TriggerID:   spec.ID,
		Source:      spec.Type,
		Payload:     stdout,
		PayloadHash: sum,
		Fields: map[string]any{
			"trigger_id":    spec.ID,
			"sentinel_hash": sum,
			"exit_code":     exitCode,
			"change_seq":    chgSeq,
		},
		ReceivedAt: now,
	}
	idemKey := fmt.Sprintf("sent_%s_%d_%s", spec.ID, chgSeq, sum[:12])
	if strings.TrimSpace(spec.IdempotencyKey) != "" {
		idemKey = DeriveIdempotencyKey(spec, ev, now)
	}
	return d.HandleEvent(ctx, spec, CoalescedEvent{
		Event:          ev,
		Count:          1,
		IdempotencyKey: idemKey,
	})
}

// HandleEvent enforces inhibitory brakes, claims the idempotency key, manages warm_branch TTL, and dispatches.
func (d *Daemon) HandleEvent(ctx context.Context, spec TriggerSpec, ev CoalescedEvent) (TriggerOutcome, error) {
	now := d.now()
	if ev.Count > 1 {
		d.CoalescedCount.Add(int64(ev.Count - 1))
	}

	brake, err := EvaluateBrakes(ctx, d.Store, d.Config, spec.Urgency, now)
	if err != nil {
		return TriggerOutcome{}, err
	}
	if brake.Inhibited {
		d.InhibitedCount.Add(1)
		return TriggerOutcome{
			TriggerID:      spec.ID,
			IdempotencyKey: ev.IdempotencyKey,
			Status:         "inhibited",
			Reason:         brake.Reason,
		}, nil
	}

	ret := effectiveRetention(spec)
	sentinelHash := ""
	if h, ok := ev.Fields["sentinel_hash"].(string); ok {
		sentinelHash = h
	}
	idemKey := ev.IdempotencyKey
	if idemKey == "" {
		idemKey = DeriveIdempotencyKey(spec, ev.Event, now)
	}

	claimed, existing, err := d.Store.TryClaimTriggerRun(ctx, store.TriggerRun{
		IdempotencyKey:   idemKey,
		TriggerID:        spec.ID,
		LastSentinelHash: sentinelHash,
		Status:           "running",
		CreatedAt:        now.Unix(),
		ExpiresAt:        now.Add(ret).Unix(),
	})
	if err != nil {
		return TriggerOutcome{}, err
	}
	if !claimed {
		d.CoalescedCount.Add(1)
		return TriggerOutcome{
			TriggerID:      spec.ID,
			IdempotencyKey: idemKey,
			Status:         "coalesced",
			Reason:         fmt.Sprintf("duplicate idempotency_key (prior status=%s)", existing.Status),
			BranchID:       existing.BranchID,
			SentinelHash:   existing.LastSentinelHash,
		}, nil
	}

	warmBranchID := d.lookupWarmBranch(spec, now)
	branchID, costUSD, runErr := d.Handler(ctx, spec, ev, warmBranchID)
	if runErr != nil {
		_ = d.Store.UpdateTriggerRun(ctx, idemKey, "failed", branchID, sentinelHash, costUSD)
		return TriggerOutcome{
			TriggerID:      spec.ID,
			IdempotencyKey: idemKey,
			Status:         "failed",
			Reason:         runErr.Error(),
			BranchID:       branchID,
			SentinelHash:   sentinelHash,
			CostUSD:        costUSD,
		}, runErr
	}

	if spec.SessionMode == "warm_branch" && branchID != "" {
		d.recordWarmBranch(spec, branchID, now)
	}
	_ = d.Store.UpdateTriggerRun(ctx, idemKey, "done", branchID, sentinelHash, costUSD)
	d.DispatchedCount.Add(1)
	return TriggerOutcome{
		TriggerID:      spec.ID,
		IdempotencyKey: idemKey,
		Status:         "done",
		BranchID:       branchID,
		SentinelHash:   sentinelHash,
		CostUSD:        costUSD,
	}, nil
}

func (d *Daemon) lookupWarmBranch(spec TriggerSpec, now time.Time) string {
	if spec.SessionMode != "warm_branch" {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ref, ok := d.warmBranches[spec.ID]
	if !ok || now.After(ref.expiresAt) {
		delete(d.warmBranches, spec.ID)
		return ""
	}
	return ref.branchID
}

func (d *Daemon) recordWarmBranch(spec TriggerSpec, branchID string, now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.warmBranches[spec.ID] = warmBranchRef{
		branchID:  branchID,
		expiresAt: now.Add(effectiveWarmTTL(spec)),
	}
}

// RunGC deletes expired trigger_runs records whose TTL retention has elapsed.
func (d *Daemon) RunGC(ctx context.Context) (int64, error) {
	return d.Store.PurgeExpiredTriggerRuns(ctx, d.now().Unix())
}

// WebhookHandler returns an HTTP handler exposing POST /webhook/{trigger_id} with HMAC + filter enforcement.
func (d *Daemon) WebhookHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		trigID := strings.TrimPrefix(r.URL.Path, "/webhook/")
		trigID = strings.Trim(trigID, "/")
		spec, ok := d.findTrigger(trigID)
		if !ok || spec.Type != "webhook" {
			http.Error(w, "unknown webhook trigger", http.StatusNotFound)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		if d.Config.WebhookSecret != "" {
			sig := r.Header.Get("X-Soma-Signature")
			if sig == "" {
				sig = r.Header.Get("X-Hub-Signature-256")
			}
			if !verifyHMACSHA256(d.Config.WebhookSecret, sig, body) {
				http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
				return
			}
		}

		var fields map[string]any
		if len(body) > 0 {
			_ = json.Unmarshal(body, &fields)
		}
		if fields == nil {
			fields = make(map[string]any)
		}
		if !matchesFilter(spec.Filter, fields) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"status": "skipped",
				"reason": "filter_mismatch",
			})
			return
		}

		ev := Event{
			TriggerID:   spec.ID,
			Source:      "webhook",
			Payload:     body,
			PayloadHash: sha256Hex(body),
			Fields:      fields,
			ReceivedAt:  d.now(),
		}
		d.Coalescer.Submit(d.effectiveCtx(), spec, ev, func(ce CoalescedEvent) {
			_, _ = d.HandleEvent(d.effectiveCtx(), spec, ce)
		})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":     "accepted",
			"trigger_id": spec.ID,
		})
	})
	return mux
}

func (d *Daemon) findTrigger(id string) (TriggerSpec, bool) {
	for _, t := range d.Config.Triggers {
		if t.ID == id {
			return t, true
		}
	}
	return TriggerSpec{}, false
}

// Start runs webhook HTTP server, fsnotify watchers, sentinel/watch/cron tickers, and TTL GC until ctx cancels.
func (d *Daemon) Start(ctx context.Context) error {
	d.mu.Lock()
	d.runCtx = ctx
	ln, err := net.Listen("tcp", d.Config.WebhookAddr)
	if err != nil {
		d.mu.Unlock()
		return fmt.Errorf("listen webhook %s: %w", d.Config.WebhookAddr, err)
	}
	d.listenAddr = ln.Addr().String()
	d.mu.Unlock()

	srv := &http.Server{
		Handler:           d.WebhookHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case errCh <- err:
			default:
			}
		}
	}()

	// Start periodic TTL GC loop.
	gcDur, err := ParseDurationExtended(d.Config.GCInterval)
	if err != nil || gcDur <= 0 {
		gcDur = 5 * time.Minute
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(gcDur)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = d.RunGC(ctx)
			}
		}
	}()

	// Start sentinel / watch / cron pollers.
	var fsTriggers []TriggerSpec
	for _, spec := range d.Config.Triggers {
		switch spec.Type {
		case "sentinel", "watch", "cron":
			sp := spec
			wg.Add(1)
			go func() {
				defer wg.Done()
				d.runPoller(ctx, sp)
			}()
		case "fsnotify":
			fsTriggers = append(fsTriggers, spec)
		}
	}

	if len(fsTriggers) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.runFSNotify(ctx, fsTriggers)
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
	case e := <-errCh:
		runErr = e
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	d.Coalescer.Stop()
	wg.Wait()
	return runErr
}

func (d *Daemon) runPoller(ctx context.Context, spec TriggerSpec) {
	interval, err := ParseDurationExtended(spec.Interval)
	if err != nil || interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if spec.Type == "cron" {
				now := d.now()
				ev := Event{
					TriggerID:   spec.ID,
					Source:      "cron",
					Payload:     []byte(spec.Prompt),
					PayloadHash: sha256Hex([]byte(fmt.Sprintf("%s|%d", spec.ID, now.Unix()/int64(maxInt64(int64(interval.Seconds()), 1))))),
					Fields:      map[string]any{"trigger_id": spec.ID},
					ReceivedAt:  now,
				}
				_, _ = d.HandleEvent(ctx, spec, CoalescedEvent{
					Event:          ev,
					Count:          1,
					IdempotencyKey: DeriveIdempotencyKey(spec, ev, now),
				})
				continue
			}
			out, _ := d.EvaluateSentinel(ctx, spec)
			if out.Status == "stopped" {
				return
			}
		}
	}
}

func (d *Daemon) runFSNotify(ctx context.Context, specs []TriggerSpec) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer func() { _ = watcher.Close() }()

	type watchTarget struct {
		spec TriggerSpec
		path string
	}
	var targets []watchTarget
	watchedDirs := make(map[string]bool)

	for _, spec := range specs {
		for _, p := range spec.Paths {
			absPath := p
			if !filepath.IsAbs(absPath) && d.WorkspaceDir != "" {
				absPath = filepath.Join(d.WorkspaceDir, p)
			}
			absPath = filepath.Clean(absPath)
			targets = append(targets, watchTarget{spec: spec, path: absPath})

			watchDir := absPath
			if info, statErr := os.Stat(absPath); statErr == nil && !info.IsDir() {
				watchDir = filepath.Dir(absPath)
			} else if errors.Is(statErr, os.ErrNotExist) {
				watchDir = filepath.Dir(absPath)
			}
			if !watchedDirs[watchDir] {
				if addErr := watcher.Add(watchDir); addErr == nil {
					watchedDirs[watchDir] = true
				}
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-watcher.Errors:
			if !ok || err != nil {
				if !ok {
					return
				}
			}
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			evPath := filepath.Clean(ev.Name)
			for _, tgt := range targets {
				if evPath == tgt.path || strings.HasPrefix(evPath, tgt.path+string(filepath.Separator)) {
					now := d.now()
					payload := []byte(fmt.Sprintf("%s:%s", ev.Op.String(), evPath))
					de := Event{
						TriggerID:   tgt.spec.ID,
						Source:      "fsnotify",
						Payload:     payload,
						PayloadHash: sha256Hex(payload),
						Fields:      map[string]any{"path": evPath, "op": ev.Op.String()},
						ReceivedAt:  now,
					}
					specCopy := tgt.spec
					d.Coalescer.Submit(ctx, specCopy, de, func(ce CoalescedEvent) {
						_, _ = d.HandleEvent(ctx, specCopy, ce)
					})
				}
			}
		}
	}
}

// DryRun simulates brake evaluation and sentinel command inspection for triggerID without mutating runs.
func (d *Daemon) DryRun(ctx context.Context, triggerID string) (string, error) {
	spec, ok := d.findTrigger(triggerID)
	if !ok {
		return "", fmt.Errorf("trigger %q not found in config", triggerID)
	}
	now := d.now()
	brake, err := EvaluateBrakes(ctx, d.Store, d.Config, spec.Urgency, now)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Dry-run trigger: %s (type=%s session_mode=%s capability=%s)\n",
		spec.ID, spec.Type, spec.SessionMode, spec.Capability)
	if brake.Inhibited {
		fmt.Fprintf(&b, "  Brakes:       INHIBITED (%s)\n", brake.Reason)
	} else {
		fmt.Fprintf(&b, "  Brakes:       CLEAR\n")
	}

	if spec.Command != "" {
		stdout, code, cmdErr := d.CommandRunner(ctx, spec.Command, d.WorkspaceDir)
		sum := sha256Hex(stdout)
		lastHash, _ := d.Store.GetLatestTriggerHash(ctx, spec.ID)
		wouldWake := sum != lastHash
		fmt.Fprintf(&b, "  Sentinel:     exit=%d err=%v sha256=%s last=%s would_wake=%v\n",
			code, cmdErr, sum[:12], truncateHash(lastHash), wouldWake)
	}
	ev := Event{
		TriggerID:   spec.ID,
		Source:      spec.Type,
		Payload:     []byte("dry-run"),
		PayloadHash: sha256Hex([]byte("dry-run")),
		Fields:      map[string]any{"trigger_id": spec.ID},
		ReceivedAt:  now,
	}
	fmt.Fprintf(&b, "  Idempotency:  %s\n", DeriveIdempotencyKey(spec, ev, now))
	return b.String(), nil
}

func matchesFilter(filter string, fields map[string]any) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	clauses := strings.Split(filter, "&&")
	for _, rawClause := range clauses {
		clause := strings.TrimSpace(rawClause)
		if clause == "" {
			continue
		}
		var op string
		if strings.Contains(clause, "!=") {
			op = "!="
		} else if strings.Contains(clause, "==") {
			op = "=="
		} else {
			return false
		}
		parts := strings.SplitN(clause, op, 2)
		key := strings.TrimSpace(parts[0])
		want := strings.Trim(strings.TrimSpace(parts[1]), `'"`)
		gotVal, ok := lookupField(fields, key)
		if !ok {
			if op == "==" {
				return false
			}
			continue
		}
		gotStr := fmt.Sprintf("%v", gotVal)
		if op == "==" && gotStr != want {
			return false
		}
		if op == "!=" && gotStr == want {
			return false
		}
	}
	return true
}

func lookupField(fields map[string]any, dotted string) (any, bool) {
	if v, ok := fields[dotted]; ok {
		return v, true
	}
	parts := strings.Split(dotted, ".")
	var cur any = fields
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		nxt, exists := m[p]
		if !exists {
			return nil, false
		}
		cur = nxt
	}
	return cur, true
}

func verifyHMACSHA256(secret, header string, body []byte) bool {
	if header == "" {
		return false
	}
	sigHex := strings.TrimPrefix(strings.TrimSpace(header), "sha256=")
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	return hmac.Equal(got, want)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func truncateHash(h string) string {
	if len(h) <= 12 {
		if h == "" {
			return "<none>"
		}
		return h
	}
	return h[:12]
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// ParseClockTime is exposed for deterministic unit tests building quiet-hour timestamps cleanly.
func ParseClockTime(hhmm string) time.Time {
	t, _ := time.Parse("15:04", hhmm)
	return t
}

// ValidateConfig checks basic invariants on loaded triggers configuration.
func ValidateConfig(cfg Config) error {
	seen := make(map[string]bool)
	for _, tr := range cfg.Triggers {
		if tr.ID == "" {
			return errors.New("trigger id must not be empty")
		}
		if seen[tr.ID] {
			return fmt.Errorf("duplicate trigger id %q", tr.ID)
		}
		seen[tr.ID] = true
		if tr.Type != "webhook" && tr.Type != "sentinel" && tr.Type != "watch" && tr.Type != "fsnotify" && tr.Type != "cron" {
			return fmt.Errorf("trigger %s has unsupported type %q", tr.ID, tr.Type)
		}
	}
	return nil
}
