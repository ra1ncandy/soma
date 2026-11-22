package embed

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	// DefaultMatryoshkaDims is the default MRL truncation dimension (256 bytes per int8 vector).
	DefaultMatryoshkaDims = 256
	// DefaultRRFK is the standard Reciprocal Rank Fusion smoothing constant (Cormack et al., 2009).
	DefaultRRFK = 60.0
	// DefaultQwen3QueryInstruct is the asymmetric query prefix for Qwen3-Embedding and jina-code-embeddings.
	DefaultQwen3QueryInstruct = "Instruct: Retrieve relevant software engineering memory rules, failed approaches, or code/log snippets\nQuery: "
)

// Embedder abstracts local or remote Matryoshka int8 embedding generation.
type Embedder interface {
	ModelID() string
	Dims() int
	Embed(ctx context.Context, texts []string, isQuery bool) ([][]byte, error)
}

// Document represents a candidate item (memory node or @page chunk) for Hybrid BM25 + Vector RRF search.
type Document struct {
	ID      string
	Text    string
	VecInt8 []byte
	Boost   float64 // optional multiplicative multiplier (default 1.0)
}

// SearchResult holds the fused RRF score and component lexical/semantic scores for a Document.
type SearchResult struct {
	ID          string
	RRFScore    float64
	BM25Score   float64
	CosineScore float64
	BM25Rank    int
	VecRank     int
}

// QuantizeMatryoshkaInt8 truncates vec to maxDims (Matryoshka Representation Learning prefix),
// L2-normalizes the truncated slice, and scales its dynamic range into signed int8 [-127, 127].
func QuantizeMatryoshkaInt8(vec []float32, maxDims int) []byte {
	if len(vec) == 0 {
		return nil
	}
	if maxDims <= 0 {
		maxDims = DefaultMatryoshkaDims
	}
	n := len(vec)
	if n > maxDims {
		n = maxDims
	}

	var normSq float64
	for i := 0; i < n; i++ {
		v := float64(vec[i])
		normSq += v * v
	}
	if normSq == 0 {
		return make([]byte, n)
	}
	invNorm := 1.0 / math.Sqrt(normSq)

	var maxAbs float64
	for i := 0; i < n; i++ {
		a := math.Abs(float64(vec[i]) * invNorm)
		if a > maxAbs {
			maxAbs = a
		}
	}
	if maxAbs == 0 {
		return make([]byte, n)
	}
	scale := 127.0 / maxAbs

	out := make([]byte, n)
	for i := 0; i < n; i++ {
		normed := float64(vec[i]) * invNorm
		q := int(math.Round(normed * scale))
		if q > 127 {
			q = 127
		} else if q < -127 {
			q = -127
		}
		out[i] = byte(int8(q))
	}
	return out
}

// CosineInt8 computes the cosine similarity in [-1.0, 1.0] between two int8-quantized Matryoshka vectors.
// If lengths differ, it evaluates over the shared Matryoshka prefix min(len(a), len(b)).
func CosineInt8(a, b []byte) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, normA, normB int64
	for i := 0; i < n; i++ {
		va := int64(int8(a[i]))
		vb := int64(int8(b[i]))
		dot += va * vb
		normA += va * va
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return float64(dot) / (math.Sqrt(float64(normA)) * math.Sqrt(float64(normB)))
}

// Tokenize extracts both exact compound identifiers (e.g. "soma_crc_7741", "timeout.yaml")
// and decomposed camelCase / snake_case / path sub-tokens for high-recall code and log BM25.
func Tokenize(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var tokens []string
	var cur []rune

	flushWord := func(word []rune) {
		if len(word) == 0 {
			return
		}
		raw := string(word)
		low := strings.ToLower(raw)
		if len(low) >= 2 || (len(low) == 1 && unicode.IsDigit(word[0])) {
			tokens = append(tokens, low)
		}
		// Decompose compound snake_case / path / dotted identifiers and camelCase.
		if strings.ContainsAny(raw, "_./-") || hasCamelTransition(word) {
			parts := splitCompound(word)
			if len(parts) > 1 {
				for _, p := range parts {
					pl := strings.ToLower(p)
					if len(pl) >= 2 && pl != low {
						tokens = append(tokens, pl)
					}
				}
			}
		}
	}

	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.' || r == '/' || r == '-' {
			cur = append(cur, r)
		} else {
			flushWord(cur)
			cur = cur[:0]
		}
	}
	flushWord(cur)
	return tokens
}

func hasCamelTransition(word []rune) bool {
	for i := 1; i < len(word); i++ {
		if unicode.IsLower(word[i-1]) && unicode.IsUpper(word[i]) {
			return true
		}
	}
	return false
}

func splitCompound(word []rune) []string {
	var parts []string
	var sub []rune
	flushSub := func() {
		if len(sub) > 0 {
			parts = append(parts, string(sub))
			sub = sub[:0]
		}
	}
	for i, r := range word {
		if r == '_' || r == '.' || r == '/' || r == '-' {
			flushSub()
			continue
		}
		if i > 0 && unicode.IsLower(word[i-1]) && unicode.IsUpper(r) {
			flushSub()
		}
		sub = append(sub, r)
	}
	flushSub()
	return parts
}

// ScoreBM25 computes Okapi BM25 (k1=1.2, b=0.75) lexical scores for query against docs.
func ScoreBM25(query string, docs []Document) map[string]float64 {
	scores := make(map[string]float64, len(docs))
	if len(docs) == 0 {
		return scores
	}
	qToks := Tokenize(query)
	if len(qToks) == 0 {
		return scores
	}

	const k1 = 1.2
	const b = 0.75

	docFreqs := make([]map[string]int, len(docs))
	docLens := make([]int, len(docs))
	df := make(map[string]int, len(qToks))
	var totalLen int

	for i, d := range docs {
		toks := Tokenize(d.Text)
		docLens[i] = len(toks)
		totalLen += len(toks)
		tf := make(map[string]int, len(toks))
		for _, t := range toks {
			tf[t]++
		}
		docFreqs[i] = tf
		for t := range tf {
			df[t]++
		}
	}

	avgDL := float64(totalLen) / float64(len(docs))
	if avgDL <= 0 {
		avgDL = 1.0
	}
	nDocs := float64(len(docs))

	// Deduplicate query terms while preserving query term frequency weight.
	qTF := make(map[string]int, len(qToks))
	for _, qt := range qToks {
		qTF[qt]++
	}

	for i, d := range docs {
		tfMap := docFreqs[i]
		dl := float64(docLens[i])
		var sum float64
		for qt, qCount := range qTF {
			tf := float64(tfMap[qt])
			if tf == 0 {
				continue
			}
			nQt := float64(df[qt])
			idf := math.Log(1.0 + (nDocs-nQt+0.5)/(nQt+0.5))
			denom := tf + k1*(1.0-b+b*(dl/avgDL))
			sum += idf * ((tf * (k1 + 1.0)) / denom) * (1.0 + 0.1*float64(qCount-1))
		}
		if sum > 0 {
			scores[d.ID] = sum
		}
	}
	return scores
}

// HybridSearch fuses Lexical BM25 and Matryoshka int8 Cosine Similarity via Reciprocal Rank Fusion (RRF).
func HybridSearch(ctx context.Context, embedder Embedder, query string, docs []Document, topK int) ([]SearchResult, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	if topK <= 0 || topK > len(docs) {
		topK = len(docs)
	}

	bm25Scores := ScoreBM25(query, docs)

	// Compute or reuse document & query int8 embeddings when embedder is provided.
	cosScores := make(map[string]float64, len(docs))
	if embedder != nil && strings.TrimSpace(query) != "" {
		qVecs, qErr := embedder.Embed(ctx, []string{query}, true)
		if qErr == nil && len(qVecs) == 1 && len(qVecs[0]) > 0 {
			qVec := qVecs[0]
			var missingIdx []int
			var missingTexts []string
			for i := range docs {
				if len(docs[i].VecInt8) == 0 {
					missingIdx = append(missingIdx, i)
					missingTexts = append(missingTexts, docs[i].Text)
				}
			}
			if len(missingTexts) > 0 {
				if dVecs, dErr := embedder.Embed(ctx, missingTexts, false); dErr == nil && len(dVecs) == len(missingIdx) {
					for j, idx := range missingIdx {
						docs[idx].VecInt8 = dVecs[j]
					}
				}
			}
			for _, d := range docs {
				if len(d.VecInt8) > 0 {
					cos := CosineInt8(qVec, d.VecInt8)
					if cos > 0.05 {
						cosScores[d.ID] = cos
					}
				}
			}
		}
	}

	// Build BM25 rank order (1-indexed).
	type pair struct {
		id    string
		score float64
	}
	var bm25List []pair
	for _, d := range docs {
		if s := bm25Scores[d.ID]; s > 0 {
			bm25List = append(bm25List, pair{id: d.ID, score: s})
		}
	}
	sort.Slice(bm25List, func(i, j int) bool {
		if bm25List[i].score != bm25List[j].score {
			return bm25List[i].score > bm25List[j].score
		}
		return bm25List[i].id < bm25List[j].id
	})
	bm25Rank := make(map[string]int, len(bm25List))
	for i, p := range bm25List {
		bm25Rank[p.id] = i + 1
	}

	// Build Vector Cosine rank order (1-indexed).
	var vecList []pair
	for _, d := range docs {
		if s, ok := cosScores[d.ID]; ok {
			vecList = append(vecList, pair{id: d.ID, score: s})
		}
	}
	sort.Slice(vecList, func(i, j int) bool {
		if vecList[i].score != vecList[j].score {
			return vecList[i].score > vecList[j].score
		}
		return vecList[i].id < vecList[j].id
	})
	vecRank := make(map[string]int, len(vecList))
	for i, p := range vecList {
		vecRank[p.id] = i + 1
	}

	// Fuse via Reciprocal Rank Fusion: RRF(d) = (1/(60 + r_bm25) + 1/(60 + r_vec)) * boost.
	var results []SearchResult
	for _, d := range docs {
		rB := bm25Rank[d.ID]
		rV := vecRank[d.ID]
		if rB == 0 && rV == 0 {
			continue
		}
		var rrf float64
		if rB > 0 {
			rrf += 1.0 / (DefaultRRFK + float64(rB))
		}
		if rV > 0 {
			rrf += 1.0 / (DefaultRRFK + float64(rV))
		}
		boost := d.Boost
		if boost <= 0 {
			boost = 1.0
		}
		rrf *= boost
		results = append(results, SearchResult{
			ID:          d.ID,
			RRFScore:    rrf,
			BM25Score:   bm25Scores[d.ID],
			CosineScore: cosScores[d.ID],
			BM25Rank:    rB,
			VecRank:     rV,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if math.Abs(results[i].RRFScore-results[j].RRFScore) > 1e-12 {
			return results[i].RRFScore > results[j].RRFScore
		}
		if math.Abs(results[i].BM25Score-results[j].BM25Score) > 1e-12 {
			return results[i].BM25Score > results[j].BM25Score
		}
		if math.Abs(results[i].CosineScore-results[j].CosineScore) > 1e-12 {
			return results[i].CosineScore > results[j].CosineScore
		}
		return results[i].ID < results[j].ID
	})

	if len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// LocalSimHashEmbedder is a zero-network, CGO_ENABLED=0 deterministic Matryoshka int8 embedder
// using signed random projections over token and character n-gram features.
type LocalSimHashEmbedder struct {
	dims int
}

// NewLocalSimHashEmbedder constructs an offline deterministic int8 embedder of dims (default 256).
func NewLocalSimHashEmbedder(dims int) *LocalSimHashEmbedder {
	if dims <= 0 {
		dims = DefaultMatryoshkaDims
	}
	return &LocalSimHashEmbedder{dims: dims}
}

func (e *LocalSimHashEmbedder) ModelID() string { return fmt.Sprintf("soma-simhash-mrl-%d", e.dims) }

func (e *LocalSimHashEmbedder) Dims() int { return e.dims }

func (e *LocalSimHashEmbedder) Embed(_ context.Context, texts []string, _ bool) ([][]byte, error) {
	out := make([][]byte, len(texts))
	for i, txt := range texts {
		out[i] = e.embedOne(txt)
	}
	return out, nil
}

func (e *LocalSimHashEmbedder) embedOne(text string) []byte {
	acc := make([]float32, e.dims)
	toks := Tokenize(text)
	if len(toks) == 0 {
		return make([]byte, e.dims)
	}

	addFeature := func(feat string, weight float32) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(feat))
		seed := h.Sum64()
		// Project each feature onto 8 sparse coordinates with deterministic Rademacher signs.
		s := seed
		for k := 0; k < 8; k++ {
			s ^= s >> 12
			s ^= s << 25
			s ^= s >> 27
			val := s * 0x2545F4914F6CDD1D
			idx := int(val % uint64(e.dims))
			sign := float32(1.0)
			if (val>>32)&1 == 1 {
				sign = -1.0
			}
			acc[idx] += sign * weight
		}
	}

	for pos, tok := range toks {
		// Full token feature + Stemmed prefix + character 3-gram features for morphological similarity.
		addFeature("tok:"+tok, 2.0)
		if len(tok) >= 5 {
			addFeature("stem:"+tok[:5], 1.2)
		}
		if len(tok) >= 4 {
			addFeature("p4:"+tok[:4], 1.0)
		}
		runes := []rune(tok)
		for j := 0; j+3 <= len(runes); j++ {
			addFeature("g3:"+string(runes[j:j+3]), 0.45)
		}
		// Bigram token feature for local phrase order.
		if pos+1 < len(toks) {
			addFeature("bi:"+tok+"_"+toks[pos+1], 1.25)
		}
	}
	return QuantizeMatryoshkaInt8(acc, e.dims)
}

// OpenAICompatEmbedder talks to any OpenAI-compatible /v1/embeddings endpoint
// (Ollama qwen3-embedding:0.6b / embeddinggemma, Mistral codestral-embed, Voyage voyage-code-3, OpenAI, etc.).
type OpenAICompatEmbedder struct {
	BaseURL        string
	APIKey         string
	Model          string
	Dimensions     int
	QueryInstruct  string
	SendDimensions bool
	HTTPClient     *http.Client
}

// NewOpenAICompatEmbedder builds a Matryoshka int8 embedding client for baseURL and model.
func NewOpenAICompatEmbedder(baseURL, apiKey, model string, dims int) *OpenAICompatEmbedder {
	if dims <= 0 {
		dims = DefaultMatryoshkaDims
	}
	instruct := ""
	lowModel := strings.ToLower(model)
	if strings.Contains(lowModel, "qwen3-embedding") || strings.Contains(lowModel, "jina-code-embeddings") {
		instruct = DefaultQwen3QueryInstruct
	}
	return &OpenAICompatEmbedder{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		APIKey:        apiKey,
		Model:         model,
		Dimensions:    dims,
		QueryInstruct: instruct,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (p *OpenAICompatEmbedder) ModelID() string { return p.Model }

func (p *OpenAICompatEmbedder) Dims() int {
	if p.Dimensions <= 0 {
		return DefaultMatryoshkaDims
	}
	return p.Dimensions
}

func (p *OpenAICompatEmbedder) Embed(ctx context.Context, texts []string, isQuery bool) ([][]byte, error) {
	if p.BaseURL == "" {
		return nil, errors.New("embed: empty BaseURL")
	}
	if len(texts) == 0 {
		return nil, nil
	}

	inputs := make([]string, len(texts))
	for i, t := range texts {
		if isQuery && p.QueryInstruct != "" && !strings.HasPrefix(t, "Instruct:") {
			inputs[i] = p.QueryInstruct + t
		} else {
			inputs[i] = t
		}
	}

	payload := map[string]any{
		"model": p.Model,
		"input": inputs,
	}
	if p.SendDimensions && p.Dimensions > 0 {
		payload["dimensions"] = p.Dimensions
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "soma/0.4")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embeddings HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("decode embeddings response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(parsed.Data))
	}

	out := make([][]byte, len(texts))
	dims := p.Dims()
	for i, item := range parsed.Data {
		idx := item.Index
		if idx < 0 || idx >= len(texts) {
			idx = i
		}
		out[idx] = QuantizeMatryoshkaInt8(item.Embedding, dims)
	}
	return out, nil
}

// Float32ToBytes encodes a float32 slice as little-endian bytes for test fixtures.
func Float32ToBytes(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}
