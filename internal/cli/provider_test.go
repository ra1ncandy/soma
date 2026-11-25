package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/dag"
)

func TestSelectProviderNeverSendsMockModelToRealAPI(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOMA_MODEL", "")
	t.Setenv("SOMA_API_BASE", "https://api.example.com/v1")
	if err := os.MkdirAll(filepath.Join(ws, ".soma"), 0o700); err != nil {
		t.Fatal(err)
	}
	// What older `soma init` wrote.
	if err := os.WriteFile(filepath.Join(ws, ".soma", "config.yaml"), []byte("version: 2\ndefault_model: mock-worker-v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectProvider(ws, false); err == nil || !strings.Contains(err.Error(), "SOMA_MODEL") {
		t.Fatalf("want a missing-model error, got %v", err)
	}
	t.Setenv("SOMA_MODEL", "real-model")
	prov, model, err := selectProvider(ws, false)
	if err != nil || model != "real-model" {
		t.Fatalf("selectProvider = %v, %q, %v", prov, model, err)
	}
	if _, ok := prov.(*dag.OpenAICompatProvider); !ok {
		t.Fatalf("want OpenAI-compatible provider, got %T", prov)
	}
}

func TestSelectProviderUnconfiguredExplainsHowToConfigure(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SOMA_API_BASE", "")
	t.Setenv("SOMA_MODEL", "")
	_, _, err := selectProvider(ws, false)
	if err == nil || !strings.Contains(err.Error(), "dashboard") {
		t.Fatalf("want guidance error, got %v", err)
	}
	if _, model, err := selectProvider(ws, true); err != nil || model != "mock-worker-v1" {
		t.Fatalf("--mock: %q %v", model, err)
	}
}
