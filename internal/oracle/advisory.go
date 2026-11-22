package oracle

import (
	"context"
	"fmt"
	"strings"

	"github.com/ra1ncandy/soma/internal/compiler"
	"github.com/ra1ncandy/soma/internal/dag"
)

// AdvisorySpec supplies context for an optional stateless cross-model review.
type AdvisorySpec struct {
	GraveyardSummary string `json:"graveyard_summary,omitempty"`
	DiffText         string `json:"diff_text,omitempty"`
}

// AdvisoryReport captures concrete counterexamples emitted by a zero-history reviewer model.
// Advisory audits NEVER alter trust score ledgers.
type AdvisoryReport struct {
	Passed         bool
	Counterexample string
	RawResponse    string
}

// RunAdvisoryAudit invokes a zero-history reviewer model to inspect only goal, graveyard, and final diff.
func RunAdvisoryAudit(
	ctx context.Context,
	provider dag.ModelProvider,
	accountID string,
	modelID string,
	goal string,
	graveyardSummary string,
	diffText string,
) (AdvisoryReport, error) {
	if provider == nil {
		return AdvisoryReport{Passed: true}, nil
	}
	snap := compiler.NewEpochSnapshot(1, compiler.EpochInput{
		SystemPrompt: "You are a zero-history adversarial reviewer. Reply strictly with CLEAN if invariants hold, or a concrete counterexample.",
		TaskGoal:     goal,
	})
	tail := compiler.LiveTailState{
		WorkspaceKind:  "advisory_review",
		ResearchStatus: fmt.Sprintf("graveyard:\n%s\ndiff:\n%s", graveyardSummary, diffText),
	}
	frame := compiler.Compile(snap, nil, tail, compiler.FrontierProfile())

	resp, err := provider.Generate(ctx, dag.GenerateRequest{
		AccountID: accountID,
		ModelID:   modelID,
		TurnIndex: 1,
		Frame:     frame,
	})
	if err != nil {
		return AdvisoryReport{}, err
	}

	text := strings.TrimSpace(resp.FinalSummary)
	if text == "" {
		text = strings.TrimSpace(resp.Reasoning)
	}
	upper := strings.ToUpper(text)
	if strings.HasPrefix(upper, "CLEAN") || upper == "OK" {
		return AdvisoryReport{Passed: true, RawResponse: text}, nil
	}
	return AdvisoryReport{
		Passed:         false,
		Counterexample: text,
		RawResponse:    text,
	}, nil
}
