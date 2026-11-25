package dag

import (
	"context"
	"sort"
	"sync"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/taint"
)

// ToolDef is a single-definition built-in tool: the model-facing schema, the Layer 0 syscall
// signature, the effect class and the handler live together, so they cannot drift apart.
// Tools defined here are merged into the wire schema list, Layer 0 and the effect classifier.
type ToolDef struct {
	Name        string
	Signature   string
	Description string
	Properties  map[string]any
	Required    []string
	Effect      taint.EffectClass
	Handler     func(ctx context.Context, r *LocalToolRunner, call ToolCall, args map[string]any) (ToolOutput, error)
	// SubagentBlocked tools are not available to delegated child agents.
	SubagentBlocked bool
}

var (
	registryMu sync.RWMutex
	registry   = map[string]ToolDef{}
)

// registerTool adds a built-in tool. It is called from init functions only, before any schema
// list is built, so the tool list stays byte-stable for the life of the process.
func registerTool(def ToolDef) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if def.Effect == "" {
		def.Effect = taint.EffectExternalWrite
	}
	registry[def.Name] = def
}

func lookupTool(name string) (ToolDef, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	d, ok := registry[name]
	return d, ok
}

func registeredTools() []ToolDef {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]ToolDef, 0, len(registry))
	for _, d := range registry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func registryWireSchemas() []map[string]any {
	var out []map[string]any
	for _, d := range registeredTools() {
		props := d.Properties
		if props == nil {
			props = map[string]any{}
		}
		req := d.Required
		if req == nil {
			req = []string{}
		}
		out = append(out, simpleTool(d.Name, d.Description, req, props))
	}
	return out
}

// CoreSyscalls is the Layer 0 syscall list: the compiler's core set plus registered tools.
func CoreSyscalls() []compiler.SyscallDef {
	out := compiler.DefaultCoreSyscalls()
	for _, d := range registeredTools() {
		out = append(out, compiler.SyscallDef{Name: d.Name, Signature: d.Signature})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
