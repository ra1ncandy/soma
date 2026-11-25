package webui

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ra1ncandy/soma/internal/desktop"
	"github.com/ra1ncandy/soma/internal/store"
)

// perfCaches holds the dashboard's read-side caches. Every tool dispatch and step notifies every
// open tab, and each tab then refetches the full state; without these caches each of those
// requests re-read every step blob of every run, re-walked every workspace, and forked
// `docker inspect`.
type perfCaches struct {
	// stateVersion advances on every notify(); a rendered state is reused until it changes.
	stateVersion atomic.Uint64

	stateMu      sync.Mutex
	stateAt      time.Time
	stateForVer  uint64
	stateJSON    []byte
	stateJSONGz  []byte
	stateHasData bool

	stepMu   sync.Mutex
	stepDTOs map[string]StepDTO        // immutable once written: keyed by step ID
	frames   map[string]FrameTokensDTO // keyed by branch head step ID
	changes  map[string]changedCount   // keyed by branch ID

	deskMu      sync.Mutex
	deskAt      time.Time
	deskState   desktop.State
	deskRunning bool

	harnessMu sync.Mutex // serialises read-modify-write cycles on harness.json

	staticMu sync.Mutex
	staticGz map[string][]byte
}

type changedCount struct {
	head string
	at   time.Time
	n    int
}

const (
	stateCacheMaxAge   = 2 * time.Second
	desktopStatusTTL   = 2 * time.Second
	changedCountMaxAge = 3 * time.Second
)

// cachedDesktopStatus avoids forking `docker inspect` for every dashboard refresh and every
// proxied noVNC asset request.
func (s *Server) cachedDesktopStatus(ctx context.Context) (desktop.State, bool) {
	c := &s.perf
	c.deskMu.Lock()
	defer c.deskMu.Unlock()
	if !c.deskAt.IsZero() && time.Since(c.deskAt) < desktopStatusTTL {
		return c.deskState, c.deskRunning
	}
	st, running, _ := desktop.NewManager().Status(ctx, s.cfg.WorkspaceDir)
	c.deskState, c.deskRunning, c.deskAt = st, running, time.Now()
	return st, running
}

func (s *Server) invalidateDesktopStatus() {
	s.perf.deskMu.Lock()
	s.perf.deskAt = time.Time{}
	s.perf.deskMu.Unlock()
}

// stepDTOCached decodes a step once; steps are immutable so the result never goes stale.
func (s *Server) stepDTOCached(st store.Step, live map[string]bool) StepDTO {
	c := &s.perf
	live[st.ID] = true
	c.stepMu.Lock()
	dto, ok := c.stepDTOs[st.ID]
	c.stepMu.Unlock()
	if ok {
		return dto
	}
	dto = s.decodeStepDTO(st)
	c.stepMu.Lock()
	if c.stepDTOs == nil {
		c.stepDTOs = make(map[string]StepDTO)
	}
	c.stepDTOs[st.ID] = dto
	c.stepMu.Unlock()
	return dto
}

// pruneStepCache drops cached steps that no longer exist (deleted runs).
func (s *Server) pruneStepCache(live map[string]bool) {
	c := &s.perf
	c.stepMu.Lock()
	for id := range c.stepDTOs {
		if !live[id] {
			delete(c.stepDTOs, id)
		}
	}
	c.stepMu.Unlock()
}

func (s *Server) frameTokensCached(headStepID string, compute func() (FrameTokensDTO, bool)) FrameTokensDTO {
	c := &s.perf
	c.stepMu.Lock()
	ft, ok := c.frames[headStepID]
	c.stepMu.Unlock()
	if ok {
		return ft
	}
	ft, ok = compute()
	if !ok {
		return ft
	}
	c.stepMu.Lock()
	if c.frames == nil || len(c.frames) > 4096 {
		c.frames = make(map[string]FrameTokensDTO)
	}
	c.frames[headStepID] = ft
	c.stepMu.Unlock()
	return ft
}

func (s *Server) changedCountCached(branchID, headStepID string, compute func() int) int {
	c := &s.perf
	c.stepMu.Lock()
	cur, ok := c.changes[branchID]
	c.stepMu.Unlock()
	if ok && cur.head == headStepID && time.Since(cur.at) < changedCountMaxAge {
		return cur.n
	}
	n := compute()
	c.stepMu.Lock()
	if c.changes == nil || len(c.changes) > 4096 {
		c.changes = make(map[string]changedCount)
	}
	c.changes[branchID] = changedCount{head: headStepID, at: time.Now(), n: n}
	c.stepMu.Unlock()
	return n
}

func (s *Server) invalidateChangedCount(branchID string) {
	s.perf.stepMu.Lock()
	delete(s.perf.changes, branchID)
	s.perf.stepMu.Unlock()
}

func gzipBytes(raw []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return buf.Bytes()
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// serveState writes /api/state, reusing the rendered document while nothing has changed. With
// several tabs open, one event produces one rebuild instead of one per tab.
func (s *Server) serveState(w http.ResponseWriter, r *http.Request) {
	c := &s.perf
	c.stateMu.Lock()
	ver := c.stateVersion.Load()
	if !c.stateHasData || c.stateForVer != ver || time.Since(c.stateAt) > stateCacheMaxAge {
		state, err := s.buildState(r.Context())
		if err != nil {
			c.stateMu.Unlock()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		raw, err := json.Marshal(state)
		if err != nil {
			c.stateMu.Unlock()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		c.stateJSON = raw
		c.stateJSONGz = gzipBytes(raw)
		c.stateForVer = ver
		c.stateAt = time.Now()
		c.stateHasData = true
	}
	raw, gz := c.stateJSON, c.stateJSONGz
	c.stateMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
		return
	}
	_, _ = w.Write(raw)
}

// writeJSONCompressed is writeJSON with gzip for clients that accept it (large payloads over a tunnel).
func writeJSONCompressed(w http.ResponseWriter, r *http.Request, status int, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil || len(raw) < 1024 || !acceptsGzip(r) {
		writeJSON(w, status, payload)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Encoding", "gzip")
	w.WriteHeader(status)
	_, _ = w.Write(gzipBytes(raw))
}

// serveStatic serves an embedded asset, gzip-compressed once and cacheable when the URL carries
// the per-build version query (index.html references assets as ?v=<version>).
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	name = path.Clean("/" + name)[1:]
	raw, err := fs.ReadFile(s.static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = http.DetectContentType(raw)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Vary", "Accept-Encoding")
	if r.URL.Query().Get("v") != "" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	compressible := strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") || strings.Contains(ctype, "json") || strings.Contains(ctype, "svg")
	if !compressible || !acceptsGzip(r) || len(raw) < 1024 {
		_, _ = w.Write(raw)
		return
	}
	c := &s.perf
	c.staticMu.Lock()
	gz, ok := c.staticGz[name]
	if !ok {
		zbuf := &bytes.Buffer{}
		zw, _ := gzip.NewWriterLevel(zbuf, gzip.BestCompression)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		gz = zbuf.Bytes()
		if c.staticGz == nil {
			c.staticGz = make(map[string][]byte)
		}
		c.staticGz[name] = gz
	}
	c.staticMu.Unlock()
	w.Header().Set("Content-Encoding", "gzip")
	_, _ = w.Write(gz)
}

// mutatesHarnessConfig reports API routes that read-modify-write harness.json. They are serialised
// so two saves (or a save and a completing OAuth flow) cannot lose each other's update.
func mutatesHarnessConfig(method, p string) bool {
	if method != http.MethodPost {
		return false
	}
	if strings.HasSuffix(p, "/test") || strings.HasSuffix(p, "/run") || strings.HasSuffix(p, "/oauth/start") {
		return false // long-running or read-only; they must not hold the config lock
	}
	return strings.HasPrefix(p, "/api/harness/") || strings.HasPrefix(p, "/api/connectors/") || p == "/api/tunnel/config"
}
