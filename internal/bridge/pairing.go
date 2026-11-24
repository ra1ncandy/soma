package bridge

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PairedSender represents an approved external user on a specific messaging channel.
type PairedSender struct {
	Channel    string    `json:"channel"`
	SenderID   string    `json:"sender_id"`
	Name       string    `json:"name,omitempty"`
	ApprovedAt time.Time `json:"approved_at"`
}

// PendingPairing represents an unapproved DM pairing request awaiting host approval.
type PendingPairing struct {
	Code      string    `json:"code"`
	Channel   string    `json:"channel"`
	SenderID  string    `json:"sender_id"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type pairingDiskState struct {
	Approved map[string]PairedSender   `json:"approved"`
	Pending  map[string]PendingPairing `json:"pending"`
}

// PairingStore manages persistent DM pairing codes and approved channel senders (0600 JSON).
type PairingStore struct {
	path string
	ttl  time.Duration
	mu   sync.Mutex
}

// NewPairingStore opens or initializes a PairingStore at <somaDir>/bridge_pairing.json.
func NewPairingStore(somaDir string) *PairingStore {
	if strings.TrimSpace(somaDir) == "" {
		somaDir = ".soma"
	}
	return &PairingStore{
		path: filepath.Join(somaDir, "bridge_pairing.json"),
		ttl:  15 * time.Minute,
	}
}

func senderKey(channel, senderID string) string {
	return strings.ToLower(strings.TrimSpace(channel)) + ":" + strings.TrimSpace(senderID)
}

func (p *PairingStore) loadLocked() (pairingDiskState, error) {
	st := pairingDiskState{
		Approved: map[string]PairedSender{},
		Pending:  map[string]PendingPairing{},
	}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	if st.Approved == nil {
		st.Approved = map[string]PairedSender{}
	}
	if st.Pending == nil {
		st.Pending = map[string]PendingPairing{}
	}
	now := time.Now()
	for code, req := range st.Pending {
		if !req.ExpiresAt.IsZero() && now.After(req.ExpiresAt) {
			delete(st.Pending, code)
		}
	}
	return st, nil
}

func (p *PairingStore) saveLocked(st pairingDiskState) error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.path, raw, 0o600)
}

// IsApproved reports whether (channel, senderID) has been paired/approved.
func (p *PairingStore) IsApproved(channel, senderID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.loadLocked()
	if err != nil {
		return false
	}
	_, ok := st.Approved[senderKey(channel, senderID)]
	return ok
}

// IssueCode returns an existing non-expired pairing code for (channel, senderID) or creates a new one.
func (p *PairingStore) IssueCode(channel, senderID, name string) (PendingPairing, error) {
	channel = strings.ToLower(strings.TrimSpace(channel))
	senderID = strings.TrimSpace(senderID)
	if channel == "" || senderID == "" {
		return PendingPairing{}, errors.New("pairing: channel and sender_id are required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.loadLocked()
	if err != nil {
		return PendingPairing{}, err
	}
	for _, req := range st.Pending {
		if req.Channel == channel && req.SenderID == senderID {
			return req, nil
		}
	}
	code, err := generatePairingCode()
	if err != nil {
		return PendingPairing{}, err
	}
	now := time.Now().UTC()
	req := PendingPairing{
		Code:      code,
		Channel:   channel,
		SenderID:  senderID,
		Name:      strings.TrimSpace(name),
		CreatedAt: now,
		ExpiresAt: now.Add(p.ttl),
	}
	st.Pending[code] = req
	if err := p.saveLocked(st); err != nil {
		return PendingPairing{}, err
	}
	return req, nil
}

// ApproveCode approves a pending pairing code and persists the sender to the approved allowlist.
func (p *PairingStore) ApproveCode(code string) (PairedSender, error) {
	norm := strings.ToUpper(strings.TrimSpace(code))
	if norm == "" {
		return PairedSender{}, errors.New("pairing: code is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.loadLocked()
	if err != nil {
		return PairedSender{}, err
	}
	req, ok := st.Pending[norm]
	if !ok {
		return PairedSender{}, fmt.Errorf("pairing: code %q not found or expired", norm)
	}
	delete(st.Pending, norm)
	paired := PairedSender{
		Channel:    req.Channel,
		SenderID:   req.SenderID,
		Name:       req.Name,
		ApprovedAt: time.Now().UTC(),
	}
	st.Approved[senderKey(req.Channel, req.SenderID)] = paired
	if err := p.saveLocked(st); err != nil {
		return PairedSender{}, err
	}
	return paired, nil
}

// Revoke removes a paired sender from the approved allowlist.
func (p *PairingStore) Revoke(channel, senderID string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.loadLocked()
	if err != nil {
		return false, err
	}
	k := senderKey(channel, senderID)
	if _, ok := st.Approved[k]; !ok {
		return false, nil
	}
	delete(st.Approved, k)
	return true, p.saveLocked(st)
}

// ListApproved returns all approved paired senders sorted by channel and sender ID.
func (p *PairingStore) ListApproved() ([]PairedSender, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]PairedSender, 0, len(st.Approved))
	for _, s := range st.Approved {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Channel != out[j].Channel {
			return out[i].Channel < out[j].Channel
		}
		return out[i].SenderID < out[j].SenderID
	})
	return out, nil
}

// ListPending returns all active pending pairing requests sorted by creation time.
func (p *PairingStore) ListPending() ([]PendingPairing, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, err := p.loadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]PendingPairing, 0, len(st.Pending))
	for _, r := range st.Pending {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func generatePairingCode() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range buf {
		if i == 4 {
			b.WriteByte('-')
		}
		b.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	return b.String(), nil
}
