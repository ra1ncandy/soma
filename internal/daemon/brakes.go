package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ra1ncandy/soma/internal/store"
)

// BrakeDecision describes whether a daemon dispatch should be suppressed by deterministic guards.
type BrakeDecision struct {
	Inhibited bool
	Reason    string
}

// EvaluateBrakes checks quiet hours, max pending inbox items, daily metered spend floor, and account cooldowns.
// Triggers marked urgency="urgent" bypass quiet hours and max_pending_inbox (while still respecting budget/cooldowns).
func EvaluateBrakes(ctx context.Context, st *store.Store, cfg Config, urgency string, now time.Time) (BrakeDecision, error) {
	urgent := strings.EqualFold(strings.TrimSpace(urgency), "urgent")

	if !urgent && strings.TrimSpace(cfg.QuietHours) != "" {
		inQuiet, err := inQuietHours(cfg.QuietHours, now)
		if err != nil {
			return BrakeDecision{}, err
		}
		if inQuiet {
			return BrakeDecision{
				Inhibited: true,
				Reason:    fmt.Sprintf("quiet_hours active (%s)", cfg.QuietHours),
			}, nil
		}
	}

	if !urgent && cfg.MaxPendingInbox > 0 {
		pending, err := st.CountPendingInbox(ctx)
		if err != nil {
			return BrakeDecision{}, fmt.Errorf("count pending inbox: %w", err)
		}
		if pending >= cfg.MaxPendingInbox {
			return BrakeDecision{
				Inhibited: true,
				Reason:    fmt.Sprintf("pending_inbox (%d) >= max_pending_inbox (%d)", pending, cfg.MaxPendingInbox),
			}, nil
		}
	}

	if cfg.DailyMeteredBudgetUSD > 0 {
		spent, err := st.TotalDailyMeteredSpend(ctx)
		if err != nil {
			return BrakeDecision{}, fmt.Errorf("read metered spend: %w", err)
		}
		if spent >= cfg.DailyMeteredBudgetUSD {
			return BrakeDecision{
				Inhibited: true,
				Reason:    fmt.Sprintf("daily_metered_spend ($%.2f) >= daily_metered_budget_usd ($%.2f)", spent, cfg.DailyMeteredBudgetUSD),
			}, nil
		}
	}

	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return BrakeDecision{}, fmt.Errorf("list accounts: %w", err)
	}
	if len(accounts) > 0 {
		nowMs := now.UnixMilli()
		allCooling := true
		for _, a := range accounts {
			if a.CooldownUntil <= nowMs {
				allCooling = false
				break
			}
		}
		if allCooling {
			return BrakeDecision{
				Inhibited: true,
				Reason:    "all_accounts_cooling_down",
			}, nil
		}
	}

	return BrakeDecision{Inhibited: false}, nil
}

func inQuietHours(window string, now time.Time) (bool, error) {
	parts := strings.Split(strings.TrimSpace(window), "-")
	if len(parts) != 2 {
		return false, fmt.Errorf("invalid quiet_hours %q (expected HH:MM-HH:MM)", window)
	}
	startMin, err := parseHHMM(parts[0])
	if err != nil {
		return false, err
	}
	endMin, err := parseHHMM(parts[1])
	if err != nil {
		return false, err
	}
	curMin := now.Hour()*60 + now.Minute()
	if startMin == endMin {
		return false, nil
	}
	if startMin < endMin {
		return curMin >= startMin && curMin < endMin, nil
	}
	// Overnight wrap (e.g., 23:00-07:30).
	return curMin >= startMin || curMin < endMin, nil
}

func parseHHMM(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid HH:MM clock value %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("invalid hour in %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid minute in %q", s)
	}
	return h*60 + m, nil
}
