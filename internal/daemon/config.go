package daemon

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config configures the Tier-0 event daemon, inhibitory brakes, webhook listener, and TTL GC.
type Config struct {
	Version               int           `yaml:"version"`
	QuietHours            string        `yaml:"quiet_hours"`
	MaxPendingInbox       int           `yaml:"max_pending_inbox"`
	DailyMeteredBudgetUSD float64       `yaml:"daily_metered_budget_usd"`
	WebhookAddr           string        `yaml:"webhook_addr"`
	WebhookSecret         string        `yaml:"webhook_secret,omitempty"`
	GCInterval            string        `yaml:"gc_interval"`
	Triggers              []TriggerSpec `yaml:"triggers"`
}

// TriggerSpec defines a single webhook, sentinel, watch, fsnotify, or cron trigger.
type TriggerSpec struct {
	ID             string   `yaml:"id"`
	Type           string   `yaml:"type"` // webhook | sentinel | watch | fsnotify | cron
	Source         string   `yaml:"source,omitempty"`
	Filter         string   `yaml:"filter,omitempty"`
	Interval       string   `yaml:"interval,omitempty"`
	Paths          []string `yaml:"paths,omitempty"`
	Command        string   `yaml:"command,omitempty"`
	WakeOn         string   `yaml:"wake_on,omitempty"` // output_hash_changed | always | nonzero
	StopCommand    string   `yaml:"stop_command,omitempty"`
	Debounce       string   `yaml:"debounce,omitempty"`
	TTL            string   `yaml:"ttl,omitempty"`
	TTLRetention   string   `yaml:"ttl_retention,omitempty"`
	IdempotencyKey string   `yaml:"idempotency_key,omitempty"`
	SessionMode    string   `yaml:"session_mode,omitempty"` // isolated | warm_branch
	Capability     string   `yaml:"capability,omitempty"`   // notify_only | draft_only
	MaxTurns       int      `yaml:"max_turns,omitempty"`
	Urgency        string   `yaml:"urgency,omitempty"` // low | normal | urgent
	Prompt         string   `yaml:"prompt,omitempty"`
	Enabled        *bool    `yaml:"enabled,omitempty"`
	NotifyChannel  string   `yaml:"notify_channel,omitempty"`
	NotifyTarget   string   `yaml:"notify_target,omitempty"`
}

// IsEnabled reports whether this trigger is active (defaults to true).
func (t TriggerSpec) IsEnabled() bool {
	if t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// DefaultConfig returns sensible RFC-0001 v2.1 defaults for Tier-0 daemon execution.
func DefaultConfig() Config {
	return Config{
		Version:         2,
		MaxPendingInbox: 3,
		WebhookAddr:     "127.0.0.1:8787",
		GCInterval:      "5m",
	}
}

// LoadConfig reads and normalizes .soma/triggers.yaml from path.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse triggers config %s: %w", path, err)
	}
	if cfg.Version == 0 {
		cfg.Version = 2
	}
	if cfg.WebhookAddr == "" {
		cfg.WebhookAddr = "127.0.0.1:8787"
	}
	if cfg.GCInterval == "" {
		cfg.GCInterval = "5m"
	}
	return cfg, nil
}

// ParseDurationExtended parses Go durations plus day suffixes (e.g., "7d", "14d").
func ParseDurationExtended(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") && !strings.HasSuffix(s, "ms") && !strings.HasSuffix(s, "us") && !strings.HasSuffix(s, "ns") {
		numPart := strings.TrimSuffix(s, "d")
		days, err := strconv.ParseFloat(numPart, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid day duration %q: %w", s, err)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(s)
}

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
