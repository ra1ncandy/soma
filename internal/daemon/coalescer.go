package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"text/template"
	"time"
)

// Event represents an incoming trigger signal prior to debouncing/coalescing.
type Event struct {
	TriggerID   string
	Source      string
	Payload     []byte
	PayloadHash string
	Fields      map[string]any
	ReceivedAt  time.Time
}

// CoalescedEvent represents one or more burst events collapsed into a single dispatchable unit.
type CoalescedEvent struct {
	Event
	Count          int
	IdempotencyKey string
}

// Coalescer debounces burst trigger events per TriggerSpec.ID.
type Coalescer struct {
	mu      sync.Mutex
	pending map[string]*pendingBucket
	wg      sync.WaitGroup
}

type pendingBucket struct {
	spec   TriggerSpec
	latest Event
	count  int
	timer  *time.Timer
}

// NewCoalescer creates a Coalescer for Tier-0 trigger debouncing.
func NewCoalescer() *Coalescer {
	return &Coalescer{
		pending: make(map[string]*pendingBucket),
	}
}

// DeriveIdempotencyKey builds an idempotency key from spec.IdempotencyKey template or deterministic hash.
func DeriveIdempotencyKey(spec TriggerSpec, ev Event, now time.Time) string {
	pHash := ev.PayloadHash
	if pHash == "" {
		sum := sha256.Sum256(ev.Payload)
		pHash = hex.EncodeToString(sum[:])
	}
	if strings.TrimSpace(spec.IdempotencyKey) != "" {
		data := make(map[string]any, len(ev.Fields)+2)
		for k, v := range ev.Fields {
			data[k] = v
		}
		if _, ok := data["trigger_id"]; !ok {
			data["trigger_id"] = spec.ID
		}
		if _, ok := data["payload_hash"]; !ok {
			data["payload_hash"] = pHash
		}
		tmpl, err := template.New("idem").Option("missingkey=zero").Parse(spec.IdempotencyKey)
		if err == nil {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err == nil {
				rendered := strings.TrimSpace(buf.String())
				if rendered != "" {
					return rendered
				}
			}
		}
	}

	bucketSec := int64(60)
	if deb := effectiveDebounce(spec); deb > time.Second {
		bucketSec = int64(deb.Seconds())
	}
	if bucketSec <= 0 {
		bucketSec = 1
	}
	bucket := now.Unix() / bucketSec
	raw := fmt.Sprintf("%s|%d|%s", spec.ID, bucket, pHash)
	sum := sha256.Sum256([]byte(raw))
	return "idem_" + hex.EncodeToString(sum[:12])
}

// Submit schedules ev to emit after spec's debounce window, collapsing burst events cleanly.
func (c *Coalescer) Submit(ctx context.Context, spec TriggerSpec, ev Event, emit func(CoalescedEvent)) {
	if ev.ReceivedAt.IsZero() {
		ev.ReceivedAt = time.Now()
	}
	if ev.PayloadHash == "" && len(ev.Payload) > 0 {
		sum := sha256.Sum256(ev.Payload)
		ev.PayloadHash = hex.EncodeToString(sum[:])
	}

	deb := effectiveDebounce(spec)
	if deb <= 0 {
		if ctx.Err() != nil {
			return
		}
		ce := CoalescedEvent{
			Event:          ev,
			Count:          1,
			IdempotencyKey: DeriveIdempotencyKey(spec, ev, ev.ReceivedAt),
		}
		emit(ce)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if b, ok := c.pending[spec.ID]; ok {
		b.latest = ev
		b.count++
		return
	}

	b := &pendingBucket{
		spec:   spec,
		latest: ev,
		count:  1,
	}
	c.pending[spec.ID] = b
	c.wg.Add(1)
	b.timer = time.AfterFunc(deb, func() {
		defer c.wg.Done()
		c.mu.Lock()
		cur, exists := c.pending[spec.ID]
		if !exists {
			c.mu.Unlock()
			return
		}
		delete(c.pending, spec.ID)
		c.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		ce := CoalescedEvent{
			Event:          cur.latest,
			Count:          cur.count,
			IdempotencyKey: DeriveIdempotencyKey(cur.spec, cur.latest, cur.latest.ReceivedAt),
		}
		emit(ce)
	})
}

// Stop cancels pending timers and waits for active timer callbacks to finish.
func (c *Coalescer) Stop() {
	c.mu.Lock()
	for id, b := range c.pending {
		if b.timer != nil && b.timer.Stop() {
			c.wg.Done()
		}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.wg.Wait()
}
