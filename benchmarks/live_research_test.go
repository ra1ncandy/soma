package benchmarks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
	"github.com/ra1ncandy/soma/internal/oracle"
	"github.com/ra1ncandy/soma/internal/sandbox"
	"github.com/ra1ncandy/soma/internal/store"
	"github.com/ra1ncandy/soma/internal/vmem"
)

// =============================================================================
// Research1 (AI/ML): Numerically Stable GQA + RoPE + Incremental KV-Cache
// =============================================================================

const research1StubPy = `"""Grouped-Query Attention (GQA) with RoPE and Incremental KV-Cache (pure Python stdlib)."""
import math


def apply_rope(vec: list[float], pos: int, base: float = 10000.0) -> list[float]:
    """Apply half-split Rotary Position Embedding to a single head vector of even dimension d.

    Half-split convention: for i in 0..(d/2 - 1), pair (vec[i], vec[i + d/2]) with
    angle theta = pos * (base ** (-2*i / d)):
      out[i]       = x1 * cos(theta) - x2 * sin(theta)
      out[i + d/2] = x1 * sin(theta) + x2 * cos(theta)
    """
    raise NotImplementedError


def forward_full(
    q: list[list[list[float]]],
    k: list[list[list[float]]],
    v: list[list[list[float]]],
    rope_base: float = 10000.0,
) -> list[list[list[float]]]:
    """Causal Grouped-Query Attention over a full sequence.

    Shapes:
      q: [seq_len][n_heads][head_dim]
      k: [seq_len][n_kv_heads][head_dim]
      v: [seq_len][n_kv_heads][head_dim]
    where n_heads % n_kv_heads == 0 and head_dim is even.
    Returns output of shape [seq_len][n_heads][head_dim].
    """
    raise NotImplementedError


class KVCache:
    """Incremental KV-cache storing RoPE-rotated keys and raw values."""

    def __init__(self, n_heads: int, n_kv_heads: int, head_dim: int, rope_base: float = 10000.0):
        self.n_heads = n_heads
        self.n_kv_heads = n_kv_heads
        self.head_dim = head_dim
        self.rope_base = rope_base

    def forward_step(
        self,
        q_t: list[list[float]],
        k_t: list[list[float]],
        v_t: list[list[float]],
    ) -> list[list[float]]:
        """Process one new token at current position pos, append to cache, return [n_heads][head_dim]."""
        raise NotImplementedError
`

const research1OraclePy = `
import math, random
from gqa_rope import apply_rope, forward_full, KVCache

def dot(a, b):
    return sum(x * y for x, y in zip(a, b))

# 1. RoPE relative position invariance: dot(rope(q, m), rope(k, n)) depends only on (m - n).
rng = random.Random(42)
q0 = [rng.gauss(0, 1) for _ in range(8)]
k0 = [rng.gauss(0, 1) for _ in range(8)]
d1 = dot(apply_rope(q0, 11), apply_rope(k0, 4))
d2 = dot(apply_rope(q0, 27), apply_rope(k0, 20))
assert abs(d1 - d2) < 1e-11, f"RoPE relative invariance violated: {d1} vs {d2}"

# 2. Norm preservation of RoPE.
n_before = math.sqrt(dot(q0, q0))
n_after = math.sqrt(dot(apply_rope(q0, 19), apply_rope(q0, 19)))
assert abs(n_before - n_after) < 1e-11, f"RoPE norm changed: {n_before} vs {n_after}"

# 3. Full vs Incremental KV-cache exact equivalence for GQA (n_heads=8, n_kv_heads=2, head_dim=8, seq_len=14).
seq_len, n_heads, n_kv_heads, head_dim = 14, 8, 2, 8
Q = [[[rng.gauss(0, 1.2) for _ in range(head_dim)] for _ in range(n_heads)] for _ in range(seq_len)]
K = [[[rng.gauss(0, 1.2) for _ in range(head_dim)] for _ in range(n_kv_heads)] for _ in range(seq_len)]
V = [[[rng.gauss(0, 1.2) for _ in range(head_dim)] for _ in range(n_kv_heads)] for _ in range(seq_len)]

full_out = forward_full(Q, K, V)
assert len(full_out) == seq_len and len(full_out[0]) == n_heads and len(full_out[0][0]) == head_dim

cache = KVCache(n_heads, n_kv_heads, head_dim)
max_err = 0.0
for t in range(seq_len):
    step_out = cache.forward_step(Q[t], K[t], V[t])
    for h in range(n_heads):
        for d in range(head_dim):
            err = abs(full_out[t][h][d] - step_out[h][d])
            if err > max_err:
                max_err = err
assert max_err < 1e-9, f"KV cache mismatch: max_err={max_err}"

# 4. Strict causality: mutating future tokens t >= 7 must not change outputs at t < 7.
K2 = [[[val for val in head] for head in tok] for tok in K]
V2 = [[[val for val in head] for head in tok] for tok in V]
for t in range(7, seq_len):
    for h in range(n_kv_heads):
        for d in range(head_dim):
            K2[t][h][d] += 99.0
            V2[t][h][d] -= 77.0
full_out2 = forward_full(Q, K2, V2)
for t in range(7):
    for h in range(n_heads):
        for d in range(head_dim):
            assert abs(full_out[t][h][d] - full_out2[t][h][d]) < 1e-12, f"causality leak at t={t}"

# 5. Numerical stability under extreme logits (+1e4 / -1e4) without NaN/Inf.
Q_big = [[[1e3 if d == 0 else 0.0 for d in range(4)] for _ in range(4)] for _ in range(4)]
K_big = [[[1e3 if d == 0 else 0.0 for d in range(4)] for _ in range(2)] for _ in range(4)]
V_big = [[[float(t + 1) for _ in range(4)] for _ in range(2)] for t in range(4)]
out_big = forward_full(Q_big, K_big, V_big)
for t in range(4):
    for h in range(4):
        for d in range(4):
            val = out_big[t][h][d]
            assert math.isfinite(val), f"non-finite output under extreme logits: {val}"

print("RESEARCH1_AI_GQA_ROPE_OK")
`

func seedResearch1Workspace(dir string) error {
	return os.WriteFile(filepath.Join(dir, "gqa_rope.py"), []byte(research1StubPy), 0o644)
}

// =============================================================================
// Research2 (Biology): Reverse-Complement Genomics Variant & Compound-Het Pipeline
// =============================================================================

func seedResearch2Workspace(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return err
	}
	// Construct a deterministic chromosome sequence with 3 genes:
	// - GENE_FWD (+ strand, 1-indexed 101..160): decoy with 1 missense + 1 common AF=0.08 variant
	// - GENE_REV (- strand, 1-indexed 301..360): TRUE PATHOGENIC compound-het (phased 0|1 nonsense + 1|0 missense, PASS, QUAL>=30, AF<0.01)
	// - GENE_CIS (+ strand, 1-indexed 501..560): decoy with 2 variants in CIS (both 1|0 on same haplotype!)
	// Plus 120 KB of realistic flanking background sequence and ~200 decoy VCF records.
	seq := make([]byte, 2000)
	bases := "ACGT"
	for i := range seq {
		seq[i] = bases[(i*7+3)%4]
	}

	// GENE_FWD (+ strand, 0-indexed 100..160): ATG GCT GGA CAA TTT ... TAA (60 bp = 20 codons)
	fwdCDS := "ATGGCTGGACAATTTGCTAAACCTGGTTACGAAGCTCTGAAACCTGGTTACGAAGCTTAA"
	copy(seq[100:160], fwdCDS)

	// GENE_REV (- strand, 0-indexed 300..360):
	// Desired 5'->3' mRNA/CDS on - strand (60 bp):
	// Codon 1 (0..2): ATG (M)
	// Codon 2 (3..5): CAG (Q) -> mutate pos 3 of CDS (C->T) => TAG (STOP, nonsense!)
	// Codon 3 (6..8): GCT (A)
	// Codon 4 (9..11): GAA (E) -> mutate pos 9 of CDS (G->A) => AAA (K, missense!)
	// Codon 5..19: GCT...
	// Codon 20 (57..59): TAA (*)
	revCDS := "ATGCAGGCTGAACCTGGTTACGAAGCTCTGAAACCTGGTTACGAAGCTCTGAAAGCTTAA"
	// Reverse complement revCDS to place on + reference strand at 0-indexed [300..360] (1-indexed 301..360)
	comp := map[byte]byte{'A': 'T', 'T': 'A', 'C': 'G', 'G': 'C'}
	revOnPlus := make([]byte, len(revCDS))
	for i := 0; i < len(revCDS); i++ {
		revOnPlus[len(revCDS)-1-i] = comp[revCDS[i]]
	}
	copy(seq[300:360], revOnPlus)
	// Note on coordinates for GENE_REV (1-indexed 301..360):
	// CDS index k (0-based from 5' end of - strand) maps to 1-indexed genomic pos = 360 - k.
	// Variant 1: CDS index 3 ('C' -> 'T' on - strand) => genomic 1-indexed pos = 360 - 3 = 357, REF on + strand = 'G', ALT on + strand = 'A'.
	//   Codon 2 changes CAG (Q) -> TAG (*): p.Q2* (nonsense), phased 0|1.
	// Variant 2: CDS index 9 ('G' -> 'A' on - strand) => genomic 1-indexed pos = 360 - 9 = 351, REF on + strand = 'C', ALT on + strand = 'T'.
	//   Codon 4 changes GAA (E) -> AAA (K): p.E4K (missense), phased 1|0.

	// GENE_CIS (+ strand, 0-indexed 500..560): decoy with 2 variants on SAME haplotype (1|0 and 1|0)
	cisCDS := "ATGCAGGCTGAACCTGGTTACGAAGCTCTGAAACCTGGTTACGAAGCTCTGAAAGCTTAA"
	copy(seq[500:560], cisCDS)

	var fasta strings.Builder
	fasta.WriteString(">chr1 synthetic_genome len=2000\n")
	for i := 0; i < len(seq); i += 80 {
		end := i + 80
		if end > len(seq) {
			end = len(seq)
		}
		fasta.Write(seq[i:end])
		fasta.WriteByte('\n')
	}
	// Add a large decoy chromosome >chr2 (100 KB) so naive full-file reads spill to @page!
	fasta.WriteString(">chr2 decoy_chrom len=100000\n")
	chunk := strings.Repeat("ACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGTACGT\n", 1250)
	fasta.WriteString(chunk)
	if err := os.WriteFile(filepath.Join(dir, "data", "genome.fasta"), []byte(fasta.String()), 0o644); err != nil {
		return err
	}

	gff := `##gff-version 3
chr1	sim	CDS	101	160	.	+	0	ID=cds_fwd;Parent=GENE_FWD;Name=GENE_FWD
chr1	sim	CDS	301	360	.	-	0	ID=cds_rev;Parent=GENE_REV;Name=GENE_REV
chr1	sim	CDS	501	560	.	+	0	ID=cds_cis;Parent=GENE_CIS;Name=GENE_CIS
`
	if err := os.WriteFile(filepath.Join(dir, "data", "annotations.gff3"), []byte(gff), 0o644); err != nil {
		return err
	}

	var vcf strings.Builder
	vcf.WriteString("##fileformat=VCFv4.2\n")
	vcf.WriteString("##INFO=<ID=AF,Number=A,Type=Float,Description=\"Allele Frequency\">\n")
	vcf.WriteString("##FORMAT=<ID=GT,Number=1,Type=String,Description=\"Phased Genotype\">\n")
	vcf.WriteString("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tPROBAND\n")
	// GENE_FWD decoys: one rare missense (0|1), one common missense AF=0.08 (1|0) -> NOT rare compound het
	vcf.WriteString("chr1\t104\trs101\tG\tA\t65.0\tPASS\tAF=0.0005\tGT\t0|1\n")
	vcf.WriteString("chr1\t110\trs102\tC\tT\t70.0\tPASS\tAF=0.0800\tGT\t1|0\n")
	// GENE_REV true compound het (phased trans: 1|0 at 351 and 0|1 at 357) + a LowQual decoy at 345
	vcf.WriteString("chr1\t345\trs300\tG\tT\t15.0\tLowQual\tAF=0.0001\tGT\t0|1\n")
	vcf.WriteString("chr1\t351\trs301\tC\tT\t82.5\tPASS\tAF=0.0003\tGT\t1|0\n")
	vcf.WriteString("chr1\t357\trs302\tG\tA\t91.2\tPASS\tAF=0.0001\tGT\t0|1\n")
	// GENE_CIS decoy: two rare protein-altering variants but BOTH on haplotype 1 (1|0 and 1|0 = cis, NOT trans compound-het!)
	vcf.WriteString("chr1\t504\trs501\tC\tT\t88.0\tPASS\tAF=0.0002\tGT\t1|0\n")
	vcf.WriteString("chr1\t510\trs502\tG\tA\t85.0\tPASS\tAF=0.0004\tGT\t1|0\n")
	// 150 intergenic/chr2 decoy variants
	for i := 1; i <= 150; i++ {
		fmt.Fprintf(&vcf, "chr2\t%d\trs_decoy_%d\tA\tG\t55.0\tPASS\tAF=0.0002\tGT\t0|1\n", i*500, i)
	}
	return os.WriteFile(filepath.Join(dir, "data", "variants.vcf"), []byte(vcf.String()), 0o644)
}

const research2OraclePy = `
import json, pathlib
p = pathlib.Path("report.json")
assert p.exists(), "missing report.json"
d = json.loads(p.read_text())
assert d.get("candidate_gene") == "GENE_REV", f"wrong candidate_gene: {d.get('candidate_gene')}"
assert d.get("strand") == "-", f"wrong strand: {d.get('strand')}"
variants = d.get("variants")
assert isinstance(variants, list) and len(variants) == 2, f"expected 2 trans variants, got {variants}"
by_pos = {v["pos"]: v for v in variants}
assert 351 in by_pos and 357 in by_pos, f"expected positions 351 and 357, got {list(by_pos.keys())}"
assert by_pos[351]["consequence"] == "missense", f"pos 351 should be missense, got {by_pos[351]}"
assert by_pos[357]["consequence"] == "nonsense", f"pos 357 should be nonsense, got {by_pos[357]}"
assert by_pos[351]["protein_change"] in ("p.E4K", "E4K", "p.Glu4Lys", "Glu4Lys"), f"pos 351 protein_change wrong: {by_pos[351]}"
assert by_pos[357]["protein_change"] in ("p.Q2*", "Q2*", "p.Q2Stop", "p.Gln2*", "p.Gln2Ter", "Gln2*", "Gln2Ter"), f"pos 357 protein_change wrong: {by_pos[357]}"
print("RESEARCH2_BIO_GENOMICS_OK")
`

// =============================================================================
// Research3 (Security): Multi-Module Vulnerability Audit & Hardening
// =============================================================================

const research3TokensPy = `import base64, hashlib, hmac, json, time

def _b64d(s: str) -> bytes:
    pad = "=" * ((4 - len(s) % 4) % 4)
    return base64.urlsafe_b64decode(s + pad)

def _b64e(b: bytes) -> str:
    return base64.urlsafe_b64encode(b).rstrip(b"=").decode()

def sign_token(payload: dict, secret: bytes) -> str:
    header = {"alg": "HS256", "typ": "JWT"}
    h_b64 = _b64e(json.dumps(header, separators=(",", ":")).encode())
    p_b64 = _b64e(json.dumps(payload, separators=(",", ":")).encode())
    sig = hmac.new(secret, f"{h_b64}.{p_b64}".encode(), hashlib.sha256).digest()
    return f"{h_b64}.{p_b64}.{_b64e(sig)}"

def verify_token(token: str, secret: bytes, expected_aud: str = "soma-api") -> dict:
    parts = token.split(".")
    if len(parts) != 3:
        raise ValueError("malformed token")
    header = json.loads(_b64d(parts[0]))
    payload = json.loads(_b64d(parts[1]))
    # BUG 1: accepts alg="none" without signature verification, uses == instead of compare_digest,
    # and ignores exp / aud claims!
    if header.get("alg") == "none":
        return payload
    expected = _b64e(hmac.new(secret, f"{parts[0]}.{parts[1]}".encode(), hashlib.sha256).digest())
    if parts[2] != expected:
        raise ValueError("invalid signature")
    return payload
`

const research3StorePy = `import sqlite3

def init_db(conn: sqlite3.Connection) -> None:
    conn.execute("CREATE TABLE IF NOT EXISTS users (username TEXT PRIMARY KEY, role TEXT NOT NULL)")
    conn.execute("INSERT OR IGNORE INTO users VALUES ('alice', 'user'), ('admin', 'admin')")
    conn.commit()

def lookup_user(conn: sqlite3.Connection, username: str):
    # BUG 2: SQL injection via f-string interpolation!
    cur = conn.execute(f"SELECT username, role FROM users WHERE username = '{username}'")
    row = cur.fetchone()
    if not row:
        return None
    return {"username": row[0], "role": row[1]}
`

const research3FilesPy = `import os

def read_tenant_file(base_dir: str, rel_path: str) -> bytes:
    # BUG 3: vulnerable to ../ traversal and sibling-prefix bypass (/base_secret vs /base)
    full = os.path.abspath(os.path.join(base_dir, rel_path))
    if not full.startswith(os.path.abspath(base_dir)):
        raise PermissionError("path escapes base_dir")
    with open(full, "rb") as f:
        return f.read()
`

const research3WebhookPy = `from urllib.parse import urlparse

def validate_webhook_url(url: str) -> str:
    # BUG 4: allows http/https to loopback, private RFC1918, link-local IMDS (169.254.169.254), and IPv6 ::1
    parsed = urlparse(url)
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        raise ValueError("invalid webhook url")
    return url
`

const research3OraclePy = `
import base64, json, os, sqlite3, tempfile, time
from auth.tokens import sign_token, verify_token
from auth.store import init_db, lookup_user
from api.files import read_tenant_file
from api.webhook import validate_webhook_url

SECRET = b"super-secret-key-2026"

# 1. Token security + valid flow
good_tok = sign_token({"sub": "alice", "aud": "soma-api", "exp": int(time.time()) + 300}, SECRET)
claims = verify_token(good_tok, SECRET, expected_aud="soma-api")
assert claims["sub"] == "alice"

# alg: none bypass must fail
h_none = base64.urlsafe_b64encode(b'{"alg":"none","typ":"JWT"}').rstrip(b"=").decode()
p_adm = base64.urlsafe_b64encode(json.dumps({"sub":"admin","aud":"soma-api","exp":int(time.time())+300}).encode()).rstrip(b"=").decode()
for bad in [
    f"{h_none}.{p_adm}.",
    sign_token({"sub": "alice", "aud": "soma-api", "exp": int(time.time()) - 10}, SECRET),
    sign_token({"sub": "alice", "aud": "wrong-aud", "exp": int(time.time()) + 300}, SECRET),
]:
    try:
        verify_token(bad, SECRET, expected_aud="soma-api")
        raise AssertionError(f"verify_token should reject {bad[:40]}")
    except ValueError:
        pass

# 2. SQLi blocked + legitimate lookup works
conn = sqlite3.connect(":memory:")
init_db(conn)
assert lookup_user(conn, "alice") == {"username": "alice", "role": "user"}
assert lookup_user(conn, "' OR '1'='1") is None, "SQL injection succeeded!"
assert lookup_user(conn, "nobody' UNION SELECT 'admin','admin' --") is None, "UNION SQLi succeeded!"

# 3. Path traversal & sibling-prefix bypass blocked + legitimate file read works
with tempfile.TemporaryDirectory() as td:
    base = os.path.join(td, "tenant")
    sibling = os.path.join(td, "tenant_secret")
    os.makedirs(base)
    os.makedirs(sibling)
    open(os.path.join(base, "ok.txt"), "wb").write(b"hello")
    open(os.path.join(sibling, "flag.txt"), "wb").write(b"SECRET_FLAG")
    assert read_tenant_file(base, "ok.txt") == b"hello"
    for bad_path in ["../tenant_secret/flag.txt", "/etc/passwd"]:
        try:
            read_tenant_file(base, bad_path)
            raise AssertionError(f"path traversal succeeded: {bad_path}")
        except (PermissionError, ValueError):
            pass

# 4. SSRF blocked + public HTTPS allowed
assert validate_webhook_url("https://hooks.example.com/notify") == "https://hooks.example.com/notify"
for ssrf in [
    "http://127.0.0.1/admin",
    "http://localhost:8080/internal",
    "http://169.254.169.254/latest/meta-data/",
    "http://10.0.0.5/secret",
    "http://[::1]/admin",
    "file:///etc/passwd",
]:
    try:
        validate_webhook_url(ssrf)
        raise AssertionError(f"SSRF should be rejected: {ssrf}")
    except ValueError:
        pass

print("RESEARCH3_SECURITY_AUDIT_OK")
`

func seedResearch3Workspace(dir string) error {
	for _, sub := range []string{"auth", "api"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "__init__.py"), []byte(""), 0o644); err != nil {
			return err
		}
	}
	files := map[string]string{
		"auth/tokens.py":  research3TokensPy,
		"auth/store.py":   research3StorePy,
		"api/files.py":    research3FilesPy,
		"api/webhook.py":  research3WebhookPy,
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func TestResearchBenchmarks_FixturesAndOracleSanity(t *testing.T) {
	py3 := resolvePython3()
	// Verify each seeded workspace initially fails its oracle before the agent fixes it.
	for _, tc := range []struct {
		name   string
		seed   func(string) error
		oracle string
	}{
		{"Research1_AI", seedResearch1Workspace, research1OraclePy},
		{"Research2_Bio", seedResearch2Workspace, research2OraclePy},
		{"Research3_Sec", seedResearch3Workspace, research3OraclePy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			if err := tc.seed(ws); err != nil {
				t.Fatalf("seed: %v", err)
			}
			cmd := exec.Command(py3, "-c", tc.oracle)
			cmd.Dir = ws
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("expected unpatched %s workspace to fail oracle, got PASS:\n%s", tc.name, string(out))
			}
		})
	}
}

func TestLiveResearch_AIBioSecurity(t *testing.T) {
	configs := loadLiveConfigs(t)
	py3 := resolvePython3()

	type researchCase struct {
		name     string
		goal     string
		seed     func(string) error
		oraclePy string
		maxTurns int
	}
	cases := []researchCase{
		{
			name:     "Research1_AI_GQARoPEKVCache",
			goal:     "Implement apply_rope, forward_full, and KVCache.forward_step in gqa_rope.py (pure Python stdlib only) following the docstrings: half-split RoPE, causal Grouped-Query Attention (n_heads % n_kv_heads == 0), log-sum-exp / max-shifted numerically stable softmax (no NaN/Inf on extreme logits), and exact equivalence (<1e-9) between full-sequence forward_full and incremental KVCache.forward_step. Write and run a thorough verification test before finishing.",
			seed:     seedResearch1Workspace,
			oraclePy: research1OraclePy,
			maxTurns: 14,
		},
		{
			name:     "Research2_Bio_GenomicsCompoundHet",
			goal:     "Analyze data/genome.fasta, data/annotations.gff3, and data/variants.vcf to identify the single pathogenic compound-heterozygous candidate gene in PROBAND. Criteria: (1) FILTER == PASS, QUAL >= 30.0, AF < 0.01; (2) Variant falls within a CDS feature in annotations.gff3 and alters the translated amino acid sequence (missense, nonsense, or frameshift) using the standard genetic code, properly handling reverse-complement translation for '-' strand genes; (3) The gene has at least two qualifying variants in TRANS (one phased 0|1 and one phased 1|0). Write report.json with schema: {\"candidate_gene\": \"...\", \"strand\": \"+\"|\"-\", \"variants\": [{\"pos\": <int>, \"ref\": \"...\", \"alt\": \"...\", \"gt\": \"...\", \"consequence\": \"missense\"|\"nonsense\"|\"frameshift\", \"protein_change\": \"p.<Old><Codon1Idx><New>\"}]}. Verify your script and report.json before finishing.",
			seed:     seedResearch2Workspace,
			oraclePy: research2OraclePy,
			maxTurns: 14,
		},
		{
			name:     "Research3_Security_ChainedVulnAudit",
			goal:     "Audit and fix all security vulnerabilities across auth/tokens.py, auth/store.py, api/files.py, and api/webhook.py without breaking legitimate functionality: (1) auth/tokens.py: reject alg!='HS256' (including 'none'), use hmac.compare_digest, enforce exp (not expired) and expected_aud; (2) auth/store.py: use parameterized SQL queries to eliminate SQL injection; (3) api/files.py: use realpath/commonpath to block ../ traversal, absolute paths, and sibling-prefix bypasses; (4) api/webhook.py: reject non-http/https schemes and any loopback, private, link-local (IMDS 169.254.169.254), reserved, multicast, or localhost/IPv6-loopback targets. Verify all fixes with tests before finishing.",
			seed:     seedResearch3Workspace,
			oraclePy: research3OraclePy,
			maxTurns: 12,
		},
	}

	for _, cfg := range configs {
		cfg := cfg
		t.Run(cfg.ModelID, func(t *testing.T) {
			for _, rc := range cases {
				rc := rc
				runSomaOnlyTrials(t, cfg, rc.name, func(t *testing.T) abRunMetrics {
					ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
					defer cancel()

					ws := t.TempDir()
					if err := rc.seed(ws); err != nil {
						t.Fatalf("seed %s: %v", rc.name, err)
					}
					st, err := store.Open(filepath.Join(ws, ".soma"))
					if err != nil {
						t.Fatalf("store.Open: %v", err)
					}
					defer func() { _ = st.Close() }()
					_ = st.UpsertAccount(ctx, store.Account{
						ID: "live", Provider: "opencode-go", ModelID: cfg.ModelID, BillingMode: "subscription", MaxConcurrent: 2,
					})

					prov := newLiveProvider(cfg)
					sbx := sandbox.NewManager()
					tools := dag.NewSandboxedToolRunner(vmem.NewManager(st), sbx)
					execDag := dag.NewExecutor(st, prov, tools, compiler.ResearchProfile())
					execDag.AccountIDs = []string{"live"}
					execDag.Verifier = oracle.NewVerifier(st, sbx)

					start := time.Now()
					runID, brID, err := execDag.StartRun(ctx, dag.RunRequest{
						Title:         rc.name,
						WorkspacePath: ws,
						GoalSpec:      rc.goal,
						AccountIDs:    []string{"live"},
					})
					if err != nil {
						t.Fatalf("StartRun: %v", err)
					}
					cmdSpec, _ := json.Marshal(oracle.CommandOracleSpec{
						Command: py3,
						Args:    []string{"-c", rc.oraclePy},
					})
					_ = st.CreateContract(ctx, store.Contract{
						ID: "ctr_" + rc.name + "_" + runID, RunID: runID, Kind: "command_oracle", SpecJSON: string(cmdSpec), Required: true,
					})

					res, execErr := execDag.ExecuteBranch(ctx, brID, rc.maxTurns)
					wall := time.Since(start).Milliseconds()

					bgCtx := context.Background()
					br, _ := st.GetBranch(bgCtx, brID)
					vCtx, vCancel := context.WithTimeout(bgCtx, 30*time.Second)
					defer vCancel()
					cmd := exec.CommandContext(vCtx, py3, "-c", rc.oraclePy)
					cmd.Dir = br.WorkspaceDir
					out, vErr := cmd.CombinedOutput()
					passed := vErr == nil && res != nil && res.Status == dag.StatusCompleted
					detail := strings.TrimSpace(string(out))
					if len(detail) > 180 {
						detail = detail[len(detail)-180:]
					}
					m := collectSomaMetrics(bgCtx, st, runID, []string{brID}, wall, passed, detail)
					if execErr != nil {
						m.ErrorText = execErr.Error()
					}
					t.Logf("[%s %s] %s", rc.name, cfg.ModelID, m.Format())
					return m
				})
			}
		})
	}
}
