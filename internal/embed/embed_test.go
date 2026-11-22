package embed_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ra1ncandy/soma/internal/embed"
)

func TestQuantizeMatryoshkaInt8AndCosine(t *testing.T) {
	// Construct a 1024-dim vector and verify MRL truncation to 256-dim int8 preserves cosine similarity.
	v1 := make([]float32, 1024)
	v2 := make([]float32, 1024)
	vOrtho := make([]float32, 1024)
	for i := 0; i < 1024; i++ {
		v1[i] = float32(math.Sin(float64(i) * 0.1))
		v2[i] = float32(math.Sin(float64(i)*0.1 + 0.05))
		vOrtho[i] = float32(math.Cos(float64(i) * 0.1))
	}

	q1 := embed.QuantizeMatryoshkaInt8(v1, 256)
	q2 := embed.QuantizeMatryoshkaInt8(v2, 256)
	qO := embed.QuantizeMatryoshkaInt8(vOrtho, 256)

	if len(q1) != 256 {
		t.Fatalf("expected 256-byte Matryoshka int8 slice, got %d", len(q1))
	}
	simClose := embed.CosineInt8(q1, q2)
	simOrtho := embed.CosineInt8(q1, qO)
	if simClose < 0.99 {
		t.Fatalf("expected near-1.0 cosine similarity for close vectors, got %.4f", simClose)
	}
	if math.Abs(simOrtho) > 0.05 {
		t.Fatalf("expected near-0.0 cosine similarity for orthogonal vectors, got %.4f", simOrtho)
	}
}

func TestCodeAwareBM25AndHybridRRF(t *testing.T) {
	ctx := context.Background()
	simEmb := embed.NewLocalSimHashEmbedder(256)

	docs := []embed.Document{
		{ID: "doc_unrelated", Text: " UI stylesheet colors and padding rules for header component"},
		{ID: "doc_exact_code", Text: "rpc_watchdog failed with ERR_CODE=SOMA_CRC_7741 in config/timeout.yaml; set REQUIRED_TIMEOUT_MS=4250"},
		{ID: "doc_semantic", Text: "When the watchdog timeout expires during RPC validation, increase the service timeout configuration in milliseconds"},
	}

	// 1. Exact identifier query ("SOMA_CRC_7741"): doc_exact_code must rank #1.
	resExact, err := embed.HybridSearch(ctx, simEmb, "SOMA_CRC_7741 timeout", docs, 3)
	if err != nil {
		t.Fatalf("HybridSearch exact: %v", err)
	}
	if len(resExact) == 0 || resExact[0].ID != "doc_exact_code" {
		t.Fatalf("expected doc_exact_code ranked #1 for SOMA_CRC_7741, got %+v", resExact)
	}

	// 2. Conceptual query ("watchdog RPC timeout configuration milliseconds"): both semantic and exact docs outrank unrelated UI doc.
	resConcept, err := embed.HybridSearch(ctx, simEmb, "watchdog RPC timeout configuration milliseconds", docs, 3)
	if err != nil {
		t.Fatalf("HybridSearch concept: %v", err)
	}
	if len(resConcept) < 2 || resConcept[0].ID == "doc_unrelated" || resConcept[1].ID == "doc_unrelated" {
		t.Fatalf("expected timeout docs to outrank doc_unrelated, got %+v", resConcept)
	}
}

func TestOpenAICompatEmbedder_Qwen3QueryInstructAndMRL(t *testing.T) {
	ctx := context.Background()

	var gotInputs []string
	var gotDims int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotInputs = body.Input
		gotDims = body.Dimensions

		// Return 512-dim float32 vectors so client MRL-truncates to 256-dim int8.
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for idx := range body.Input {
			vec := make([]float32, 512)
			for j := range vec {
				vec[j] = float32(j+1) * float32(idx+1)
			}
			data = append(data, item{Index: idx, Embedding: vec})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer ts.Close()

	client := embed.NewOpenAICompatEmbedder(ts.URL+"/v1", "test-key", "qwen3-embedding:0.6b", 256)
	client.SendDimensions = true

	// Query mode (isQuery=true) should auto-prepend Qwen3 Instruct prefix.
	qVecs, err := client.Embed(ctx, []string{"how to handle HTTP 429 cooldown"}, true)
	if err != nil {
		t.Fatalf("Embed query: %v", err)
	}
	if len(qVecs) != 1 || len(qVecs[0]) != 256 {
		t.Fatalf("expected 1x256 int8 vector, got len=%d", len(qVecs))
	}
	if len(gotInputs) != 1 || !strings.HasPrefix(gotInputs[0], "Instruct:") {
		t.Fatalf("expected Qwen3 Instruct prefix on query, got %q", gotInputs)
	}
	if gotDims != 256 {
		t.Fatalf("expected dimensions=256 in wire payload, got %d", gotDims)
	}

	// Document mode (isQuery=false) should NOT prepend Instruct prefix.
	_, err = client.Embed(ctx, []string{"raw document text"}, false)
	if err != nil {
		t.Fatalf("Embed doc: %v", err)
	}
	if len(gotInputs) != 1 || strings.HasPrefix(gotInputs[0], "Instruct:") {
		t.Fatalf("expected raw document input without Instruct prefix, got %q", gotInputs)
	}
}
