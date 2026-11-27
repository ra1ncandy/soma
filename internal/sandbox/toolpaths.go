package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ToolMount is a host path the operator chose to make visible, read-only, inside the sandbox.
// By default sandboxed commands see only /usr (and its usrmerge aliases); toolchains installed
// anywhere else (a virtualenv, ~/.cargo, an application's /etc directory) are invisible.
type ToolMount struct {
	Host string // absolute host path
	Dest string // absolute path inside the sandbox (defaults to Host)
}

const (
	// EnvToolMounts lists extra read-only mounts: "host[:dest],host[:dest],...".
	EnvToolMounts = "SOMA_SANDBOX_RO_BINDS"
	// EnvToolEnv lists extra environment for sandboxed commands: "NAME=value;NAME=value".
	EnvToolEnv = "SOMA_SANDBOX_ENV"
)

// credentialDirs are home-relative paths that must never be exposed, even read-only.
var credentialDirs = []string{".ssh", ".gnupg", ".aws", ".kube", ".docker", ".netrc", ".soma", ".config/gcloud", ".config/gh", ".claude"}

// reservedDests are sandbox paths a tool mount may not replace or sit inside.
var reservedDests = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/proc", "/dev", "/tmp", "/workspace", rpcSandboxDir}

func within(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// ParseToolMounts validates an EnvToolMounts value. It fails closed: one bad entry rejects the
// whole list, so a typo cannot silently widen or narrow what the sandbox sees.
func ParseToolMounts(value, home string) ([]ToolMount, error) {
	var out []ToolMount
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		host, dest, hasDest := strings.Cut(raw, ":")
		if !hasDest {
			dest = host
		}
		if !filepath.IsAbs(host) || !filepath.IsAbs(dest) {
			return nil, fmt.Errorf("sandbox mount %q: paths must be absolute", raw)
		}
		host, dest = filepath.Clean(host), filepath.Clean(dest)
		resolved, err := filepath.EvalSymlinks(host)
		if err != nil {
			return nil, fmt.Errorf("sandbox mount %q: %w", raw, err)
		}
		for _, p := range []string{host, resolved} {
			if p == "/" || (home != "" && within(home, p)) {
				return nil, fmt.Errorf("sandbox mount %q: refusing to expose %s (it contains the home directory)", raw, p)
			}
			for _, c := range credentialDirs {
				if home != "" && (within(p, filepath.Join(home, c)) || within(filepath.Join(home, c), p)) {
					return nil, fmt.Errorf("sandbox mount %q: refusing to expose credentials under %s", raw, filepath.Join(home, c))
				}
			}
			for _, seg := range strings.Split(p, "/") {
				if seg == ".soma" {
					return nil, fmt.Errorf("sandbox mount %q: refusing to expose soma state", raw)
				}
			}
		}
		if dest == "/" {
			return nil, fmt.Errorf("sandbox mount %q: destination cannot be /", raw)
		}
		for _, r := range reservedDests {
			if within(dest, r) {
				return nil, fmt.Errorf("sandbox mount %q: destination %s is reserved", raw, dest)
			}
		}
		out = append(out, ToolMount{Host: host, Dest: dest})
	}
	return out, nil
}

// ParseToolEnv validates an EnvToolEnv value ("NAME=value;NAME=value"). Names that look like
// credentials are rejected rather than silently dropped.
func ParseToolEnv(value string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range strings.Split(value, ";") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("sandbox env %q: want NAME=value", kv)
		}
		if IsSecretEnvKey(k) {
			return nil, fmt.Errorf("sandbox env %q: name looks like a credential", k)
		}
		out[k] = v
	}
	return out, nil
}

type operatorTools struct {
	mounts []ToolMount
	env    map[string]string
	err    error
}

var (
	operatorToolsOnce sync.Once
	operatorToolsVal  operatorTools
)

// loadOperatorTools reads the operator's tool mounts and environment once per process.
func loadOperatorTools() operatorTools {
	operatorToolsOnce.Do(func() {
		home, _ := os.UserHomeDir()
		var t operatorTools
		t.mounts, t.err = ParseToolMounts(os.Getenv(EnvToolMounts), home)
		if t.err == nil {
			t.env, t.err = ParseToolEnv(os.Getenv(EnvToolEnv))
		}
		operatorToolsVal = t
	})
	return operatorToolsVal
}

// withOperatorTools adds the operator's tool mounts and environment to spec. The command's own
// environment wins over the operator's. A malformed setting is an error, never ignored.
func withOperatorTools(spec ExecSpec) (ExecSpec, error) {
	t := loadOperatorTools()
	if t.err != nil {
		return spec, errors.Join(ErrSandboxUnavailable, t.err)
	}
	if len(t.mounts) > 0 {
		spec.ToolMounts = append(append([]ToolMount(nil), spec.ToolMounts...), t.mounts...)
	}
	if len(t.env) > 0 {
		env := make(map[string]string, len(t.env)+len(spec.ExtraEnv))
		for k, v := range t.env {
			env[k] = v
		}
		for k, v := range spec.ExtraEnv {
			env[k] = v
		}
		spec.ExtraEnv = env
	}
	return spec, nil
}

var (
	identityOnce sync.Once
	identityDir  string
)

// identityFiles returns host paths of a minimal passwd and group file describing only the
// current uid/gid as user "soma". Programs that look up the current user (LibreOffice, git,
// Python's getpass) fail without an entry; binding the host's files would leak its account
// names. Returns empty strings when the files cannot be written.
func identityFiles() (passwd, group string) {
	identityOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), fmt.Sprintf("soma-sandbox-identity-%d", os.Getuid()))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		pw := fmt.Sprintf("soma:x:%d:%d:soma:/tmp:/bin/sh\n", os.Getuid(), os.Getgid())
		gr := fmt.Sprintf("soma:x:%d:\n", os.Getgid())
		if os.WriteFile(filepath.Join(dir, "passwd"), []byte(pw), 0o644) != nil ||
			os.WriteFile(filepath.Join(dir, "group"), []byte(gr), 0o644) != nil {
			return
		}
		identityDir = dir
	})
	if identityDir == "" {
		return "", ""
	}
	return filepath.Join(identityDir, "passwd"), filepath.Join(identityDir, "group")
}
