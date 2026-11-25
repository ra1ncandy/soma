package arena

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Summary aggregates one contestant's measured results.
type Summary struct {
	Contestant  string  `json:"contestant"`
	Version     string  `json:"version"`
	Attempts    int     `json:"attempts"`
	Passed      int     `json:"passed"`
	PassRate    float64 `json:"pass_rate"`
	CILow       float64 `json:"ci95_low"`
	CIHigh      float64 `json:"ci95_high"`
	MedianCalls float64 `json:"median_model_calls"`
	MedianTok   float64 `json:"median_tokens"`
	TotalCost   float64 `json:"total_cost_usd"`
	MedianWall  float64 `json:"median_wall_seconds"`
}

// Report is the full, measured-only arena output.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	Model       string    `json:"model"`
	Upstream    string    `json:"upstream_host"`
	Tasks       []string  `json:"tasks"`
	Repeats     int       `json:"repeats"`
	Notes       []string  `json:"notes"`
	Summaries   []Summary `json:"summaries"`
	Results     []Result  `json:"results"`
}

// Summarize builds per-contestant summaries with Wilson 95% intervals.
func Summarize(results []Result, versions map[string]string) []Summary {
	by := map[string][]Result{}
	for _, r := range results {
		by[r.Contestant] = append(by[r.Contestant], r)
	}
	var out []Summary
	for name, rs := range by {
		s := Summary{Contestant: name, Version: versions[name], Attempts: len(rs)}
		var calls, toks, wall []float64
		for _, r := range rs {
			if r.Passed {
				s.Passed++
			}
			calls = append(calls, float64(r.Usage.Calls))
			toks = append(toks, float64(r.Usage.InputTokens+r.Usage.OutputTokens))
			wall = append(wall, r.WallSec)
			s.TotalCost += r.Usage.CostUSD
		}
		s.PassRate = float64(s.Passed) / float64(s.Attempts)
		s.CILow, s.CIHigh = wilson(s.Passed, s.Attempts)
		s.MedianCalls, s.MedianTok, s.MedianWall = median(calls), median(toks), median(wall)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Contestant < out[j].Contestant })
	return out
}

func wilson(k, n int) (float64, float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.96
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	c := (p + z*z/(2*nf)) / den
	h := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return math.Max(0, c-h), math.Min(1, c+h)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

// Markdown renders the report. Every number in it comes from Results.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# soma vs. other harnesses — measured arena run\n\n")
	fmt.Fprintf(&b, "- Generated: %s\n- Model: `%s` (via %s, through the arena metering proxy)\n- Tasks: %s\n- Repeats: %d\n\n",
		r.GeneratedAt.UTC().Format(time.RFC3339), r.Model, r.Upstream, strings.Join(r.Tasks, ", "), r.Repeats)
	b.WriteString("| Contestant | Version | Passed | Pass rate (95% CI) | Median model calls | Median tokens | Total cost | Median wall |\n|---|---|---:|---|---:|---:|---:|---:|\n")
	for _, s := range r.Summaries {
		fmt.Fprintf(&b, "| %s | %s | %d/%d | %.0f%% (%.0f–%.0f%%) | %.0f | %.0f | $%.4f | %.0fs |\n",
			s.Contestant, s.Version, s.Passed, s.Attempts, 100*s.PassRate, 100*s.CILow, 100*s.CIHigh, s.MedianCalls, s.MedianTok, s.TotalCost, s.MedianWall)
	}
	b.WriteString("\n## Per attempt\n\n| Task | Contestant | # | Pass | Calls | In tok | Cached | Out tok | Cost | Wall | Note |\n|---|---|---:|---|---:|---:|---:|---:|---:|---:|---|\n")
	for _, x := range r.Results {
		note := x.ExitError
		if x.TimedOut {
			note = "timed out"
		}
		if len(note) > 60 {
			note = note[:57] + "..."
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %v | %d | %d | %d | %d | $%.4f | %.0fs | %s |\n",
			x.Task, x.Contestant, x.Repeat, x.Passed, x.Usage.Calls, x.Usage.InputTokens, x.Usage.CachedTokens, x.Usage.OutputTokens, x.Usage.CostUSD, x.WallSec, strings.ReplaceAll(note, "|", "/"))
	}
	if len(r.Notes) > 0 {
		b.WriteString("\n## Conditions\n\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}
