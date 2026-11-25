package dag

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// ModelInfo is operator-supplied metadata for a model: prices (USD per million tokens) so cost
// accrues and budgets/brakes work for providers that do not report cost, and the context window
// so epochs roll over before the model's real limit. It lives in pricing.yaml (project .soma,
// then ~/.soma) because prices change too often to hard-code:
//
//	models:
//	  - match: "claude-sonnet-*"     # glob on the model id
//	    input: 3.0
//	    output: 15.0
//	    cache_read: 0.3
//	    cache_write: 3.75
//	    context_window: 200000
type ModelInfo struct {
	Match         string  `yaml:"match"`
	Input         float64 `yaml:"input"`
	Output        float64 `yaml:"output"`
	CacheRead     float64 `yaml:"cache_read"`
	CacheWrite    float64 `yaml:"cache_write"`
	ContextWindow int     `yaml:"context_window"`
}

// ModelCatalog resolves ModelInfo by model id; the first matching entry wins.
type ModelCatalog struct {
	Models []ModelInfo `yaml:"models"`
}

// LoadModelCatalog reads <somaDir>/pricing.yaml then ~/.soma/pricing.yaml (project entries first).
func LoadModelCatalog(somaDir string) *ModelCatalog {
	cat := &ModelCatalog{}
	var paths []string
	if somaDir != "" {
		paths = append(paths, filepath.Join(somaDir, "pricing.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".soma", "pricing.yaml"))
	}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var c ModelCatalog
		if yaml.Unmarshal(raw, &c) == nil {
			cat.Models = append(cat.Models, c.Models...)
		}
	}
	return cat
}

// Lookup returns the first entry whose glob matches modelID (case-insensitive).
func (c *ModelCatalog) Lookup(modelID string) (ModelInfo, bool) {
	if c == nil {
		return ModelInfo{}, false
	}
	id := strings.ToLower(strings.TrimSpace(modelID))
	for _, m := range c.Models {
		pat := strings.ToLower(strings.TrimSpace(m.Match))
		if pat == "" {
			continue
		}
		if ok, _ := path.Match(pat, id); ok || pat == id {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// Cost prices a turn's usage. InputTokens are uncached input tokens.
func (m ModelInfo) Cost(input, cacheRead, cacheWrite, output int) float64 {
	return (float64(input)*m.Input + float64(cacheRead)*m.CacheRead + float64(cacheWrite)*m.CacheWrite + float64(output)*m.Output) / 1e6
}

var (
	catalogMu    sync.Mutex
	catalogCache = map[string]*ModelCatalog{}
)

// modelCatalogFor caches the catalog per .soma directory for the life of the process.
func modelCatalogFor(somaDir string) *ModelCatalog {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if c, ok := catalogCache[somaDir]; ok {
		return c
	}
	c := LoadModelCatalog(somaDir)
	catalogCache[somaDir] = c
	return c
}

// epochCeilingFor caps the profile's rollover ceiling at 70% of the model's context window, so a
// small local model rolls over before overflowing instead of failing with a context-length error.
func epochCeilingFor(profileCeiling int, info ModelInfo, found bool) int {
	if !found || info.ContextWindow <= 0 {
		return profileCeiling
	}
	limit := info.ContextWindow * 7 / 10
	if profileCeiling <= 0 || limit < profileCeiling {
		return limit
	}
	return profileCeiling
}
