package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests must never write the operator's real ~/.soma/mcp_trust.json.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "soma-mcp-trust-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("SOMA_MCP_TRUST_FILE", filepath.Join(dir, "mcp_trust.json"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
