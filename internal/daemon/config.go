package daemon

import (
	"time"

	"github.com/ra1ncandy/soma/internal/triggers"
)

// The triggers.yaml model and schedule parser live in internal/triggers so every writer
// (daemon, dashboard, the agent's cronjob_manage tool) round-trips the same struct.
type (
	Config      = triggers.Config
	TriggerSpec = triggers.TriggerSpec
	Schedule    = triggers.Schedule
)

var (
	DefaultConfig         = triggers.DefaultConfig
	LoadConfig            = triggers.LoadConfig
	ParseDurationExtended = triggers.ParseDurationExtended
	ParseSchedule         = triggers.ParseSchedule
	ValidateFilter        = triggers.ValidateFilter
)

func effectiveRetention(spec TriggerSpec) time.Duration {
	if spec.TTLRetention != "" {
		if d, err := ParseDurationExtended(spec.TTLRetention); err == nil && d > 0 {
			return d
		}
	}
	return 7 * 24 * time.Hour
}

func effectiveWarmTTL(spec TriggerSpec) time.Duration {
	if spec.TTL != "" {
		if d, err := ParseDurationExtended(spec.TTL); err == nil && d > 0 {
			return d
		}
	}
	return 45 * time.Minute
}

func effectiveDebounce(spec TriggerSpec) time.Duration {
	if spec.Debounce != "" {
		if d, err := ParseDurationExtended(spec.Debounce); err == nil && d > 0 {
			return d
		}
	}
	return 0
}
