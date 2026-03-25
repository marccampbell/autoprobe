package optimizer

import (
	"fmt"
	"strings"
	"time"
)

// RunState tracks the state of an optimization run
type RunState struct {
	EndpointName string
	StartTime    time.Time
	Iteration    int
	BaselineP95  float64
	CurrentP95   float64
	Attempts     []Attempt
}

// Attempt represents a single optimization attempt
type Attempt struct {
	Iteration  int
	Hypothesis string
	Change     string  // human-readable description
	File       string  // file that was modified
	Diff       string  // the actual diff
	P95Before  float64
	P95After   float64
	Kept       bool
}

// NewRunState creates a new run state
func NewRunState(endpointName string, baselineP95 float64) *RunState {
	return &RunState{
		EndpointName: endpointName,
		StartTime:    time.Now(),
		Iteration:    0,
		BaselineP95:  baselineP95,
		CurrentP95:   baselineP95,
		Attempts:     []Attempt{},
	}
}

// RecordAttempt records the result of an optimization attempt
func (s *RunState) RecordAttempt(hypothesis, change, file, diff string, p95Before, p95After float64, kept bool) {
	s.Attempts = append(s.Attempts, Attempt{
		Iteration:  s.Iteration,
		Hypothesis: hypothesis,
		Change:     change,
		File:       file,
		Diff:       diff,
		P95Before:  p95Before,
		P95After:   p95After,
		Kept:       kept,
	})

	if kept {
		s.CurrentP95 = p95After
	}
}

// SuccessfulAttempts returns attempts that improved performance
func (s *RunState) SuccessfulAttempts() []Attempt {
	var successful []Attempt
	for _, a := range s.Attempts {
		if a.Kept {
			successful = append(successful, a)
		}
	}
	return successful
}

// FailedAttempts returns attempts that didn't help
func (s *RunState) FailedAttempts() []Attempt {
	var failed []Attempt
	for _, a := range s.Attempts {
		if !a.Kept {
			failed = append(failed, a)
		}
	}
	return failed
}

// TotalImprovement returns the total p95 improvement from baseline
func (s *RunState) TotalImprovement() float64 {
	return s.BaselineP95 - s.CurrentP95
}

// FormatHistory returns a formatted string of attempt history for the LLM
func (s *RunState) FormatHistory() string {
	if len(s.Attempts) == 0 {
		return "No optimization attempts yet."
	}

	var sb strings.Builder
	sb.WriteString("## Previous Attempts\n\n")

	for _, a := range s.Attempts {
		status := "✗ reverted"
		if a.Kept {
			status = "✓ kept"
		}

		improvement := a.P95Before - a.P95After
		sb.WriteString(fmt.Sprintf("### Iteration %d: %s\n", a.Iteration, status))
		sb.WriteString(fmt.Sprintf("- **Hypothesis**: %s\n", a.Hypothesis))
		sb.WriteString(fmt.Sprintf("- **Change**: %s\n", a.Change))
		sb.WriteString(fmt.Sprintf("- **File**: %s\n", a.File))
		sb.WriteString(fmt.Sprintf("- **Result**: p95 %.1fms → %.1fms (%+.1fms)\n", a.P95Before, a.P95After, -improvement))
		sb.WriteString("\n")
	}

	return sb.String()
}

// FormatSummary returns a summary of the current state
func (s *RunState) FormatSummary() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Current State\n\n"))
	sb.WriteString(fmt.Sprintf("- **Baseline p95**: %.1fms\n", s.BaselineP95))
	sb.WriteString(fmt.Sprintf("- **Current p95**: %.1fms\n", s.CurrentP95))
	sb.WriteString(fmt.Sprintf("- **Total improvement**: %.1fms\n", s.TotalImprovement()))
	sb.WriteString(fmt.Sprintf("- **Iterations**: %d\n", s.Iteration))
	sb.WriteString(fmt.Sprintf("- **Successful changes**: %d\n", len(s.SuccessfulAttempts())))
	sb.WriteString(fmt.Sprintf("- **Failed attempts**: %d\n", len(s.FailedAttempts())))

	return sb.String()
}
