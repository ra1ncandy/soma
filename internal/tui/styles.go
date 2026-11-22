package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

type styleset struct {
	headerBar  lipgloss.Style
	tabActive  lipgloss.Style
	tabIdle    lipgloss.Style
	panel      lipgloss.Style
	title      lipgloss.Style
	muted      lipgloss.Style
	footer     lipgloss.Style
	statusBar  lipgloss.Style
	badgeOK    lipgloss.Style
	badgeWarn  lipgloss.Style
	badgeErr   lipgloss.Style
	badgeInfo  lipgloss.Style
	cursorRow  lipgloss.Style
}

func defaultStyles() styleset {
	return styleset{
		headerBar: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#E5E9F0")),
		tabActive: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0F172A")).
			Background(lipgloss.Color("#38BDF8")).
			Padding(0, 1),
		tabIdle: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#94A3B8")).
			Padding(0, 1),
		panel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#334155")).
			Padding(0, 1),
		title: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7DD3FC")),
		muted: lipgloss.NewStyle().Foreground(lipgloss.Color("#64748B")),
		footer: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#94A3B8")),
		statusBar: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FDE047")),
		badgeOK: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#22C55E")),
		badgeWarn: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F59E0B")),
		badgeErr: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#EF4444")),
		badgeInfo: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#38BDF8")),
		cursorRow: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#F8FAFC")).
			Background(lipgloss.Color("#1E293B")),
	}
}

func (s styleset) statusBadge(status string) string {
	up := strings.ToUpper(status)
	switch up {
	case "COMPLETED", "CLEAN", "VERIFIED", "ACTIVE", "DONE", "READY", "APPROVED":
		return s.badgeOK.Render("[" + up + "]")
	case "SUSPENDED_QUOTA", "SUSPENDED_APPROVAL", "SUSPENDED_RECONCILE", "PROPOSED", "WARN", "PENDING", "COALESCED", "STALLED":
		return s.badgeWarn.Render("[" + up + "]")
	case "FAILED", "ABANDONED", "BLOCKED", "SUPERSEDED", "ARCHIVED":
		return s.badgeErr.Render("[" + up + "]")
	default:
		return s.badgeInfo.Render("[" + up + "]")
	}
}
