package dag

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// ErrAllAccountsCoolingDown indicates every eligible account is currently under a rate-limit cooldown.
type ErrAllAccountsCoolingDown struct {
	EarliestResumeAt time.Time
}

func (e *ErrAllAccountsCoolingDown) Error() string {
	return fmt.Sprintf("all accounts cooling down until %s", e.EarliestResumeAt.Format(time.RFC3339Nano))
}

// QuotaScheduler coordinates account leases, cooldowns, and sticky-epoch affinity over SQLite.
type QuotaScheduler struct {
	store *store.Store
}

// NewQuotaScheduler returns a QuotaScheduler backed by st.
func NewQuotaScheduler(st *store.Store) *QuotaScheduler {
	return &QuotaScheduler{store: st}
}

// Acquire selects an eligible account (preferring preferredAccountID for sticky KV-cache reuse)
// and atomically claims a concurrency lease.
func (q *QuotaScheduler) Acquire(ctx context.Context, preferredAccountID string, candidateIDs []string) (store.Account, error) {
	accounts, err := q.store.ListAccounts(ctx)
	if err != nil {
		return store.Account{}, err
	}
	if len(accounts) == 0 {
		return store.Account{}, store.ErrNotFound
	}

	byID := make(map[string]store.Account, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}

	var ordered []store.Account
	seen := make(map[string]bool)
	if preferredAccountID != "" {
		if a, ok := byID[preferredAccountID]; ok {
			ordered = append(ordered, a)
			seen[a.ID] = true
		}
	}
	for _, cid := range candidateIDs {
		if seen[cid] {
			continue
		}
		if a, ok := byID[cid]; ok {
			ordered = append(ordered, a)
			seen[a.ID] = true
		}
	}
	if len(candidateIDs) == 0 {
		for _, a := range accounts {
			if !seen[a.ID] {
				ordered = append(ordered, a)
				seen[a.ID] = true
			}
		}
	}

	nowMs := time.Now().UnixMilli()
	var earliestCooldown int64
	coolingCount := 0

	for _, acc := range ordered {
		err := q.store.AcquireLease(ctx, acc.ID, nowMs)
		if err == nil {
			// Refresh account snapshot after lease acquisition.
			fresh, getErr := q.store.GetAccount(ctx, acc.ID)
			if getErr == nil {
				return fresh, nil
			}
			return acc, nil
		}
		if errors.Is(err, store.ErrNoCapacity) {
			latest, getErr := q.store.GetAccount(ctx, acc.ID)
			if getErr == nil && latest.CooldownUntil > nowMs {
				coolingCount++
				if earliestCooldown == 0 || latest.CooldownUntil < earliestCooldown {
					earliestCooldown = latest.CooldownUntil
				}
			}
		}
	}

	if coolingCount > 0 && coolingCount == len(ordered) {
		return store.Account{}, &ErrAllAccountsCoolingDown{
			EarliestResumeAt: time.UnixMilli(earliestCooldown),
		}
	}
	return store.Account{}, store.ErrNoCapacity
}

// Release releases a previously acquired lease and records metered spend.
func (q *QuotaScheduler) Release(ctx context.Context, accountID string, spentUSD float64) error {
	return q.store.ReleaseLease(ctx, accountID, spentUSD)
}

// RecordRateLimit sets a cooldown window on accountID after an upstream HTTP 429.
func (q *QuotaScheduler) RecordRateLimit(ctx context.Context, accountID string, retryAfter time.Duration, reason string) error {
	if retryAfter <= 0 {
		retryAfter = 15 * time.Second
	}
	untilMs := time.Now().Add(retryAfter).UnixMilli()
	return q.store.SetCooldown(ctx, accountID, untilMs, reason)
}
