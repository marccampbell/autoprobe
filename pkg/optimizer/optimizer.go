package optimizer

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/marccampbell/autoprobe/pkg/benchmark"
	"github.com/marccampbell/autoprobe/pkg/claude"
	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/marccampbell/autoprobe/pkg/tools"
)

// Optimizer runs the optimization loop
type Optimizer struct {
	cfg      *config.Config
	endpoint *config.EndpointConfig
	name     string
	state    *RunState
	client   *claude.Client
	dryRun   bool
}

// New creates a new optimizer
func New(cfg *config.Config, endpointName string, endpoint *config.EndpointConfig, dryRun bool) (*Optimizer, error) {
	client, err := claude.NewClient()
	if err != nil {
		return nil, err
	}

	return &Optimizer{
		cfg:      cfg,
		endpoint: endpoint,
		name:     endpointName,
		client:   client,
		dryRun:   dryRun,
	}, nil
}

// Run executes the optimization loop
func (o *Optimizer) Run(maxIterations int) error {
	// Initial benchmark
	fmt.Println("\n=== Initial Benchmark ===")
	baseline, err := o.runBenchmark()
	if err != nil {
		return fmt.Errorf("initial benchmark failed: %w", err)
	}
	benchmark.PrintStats(baseline)

	// Initialize state
	o.state = NewRunState(o.name, baseline.P95Ms)

	// Check if already meeting target
	if o.endpoint.Target.Duration() > 0 && baseline.TargetMet {
		fmt.Printf("\n✓ Already meeting target! p95 (%.1fms) is under target (%dms)\n",
			baseline.P95Ms, baseline.TargetMs)
		return nil
	}

	// Gather initial context
	fmt.Println("\n=== Gathering Context ===")
	context, err := o.gatherContext()
	if err != nil {
		return fmt.Errorf("failed to gather context: %w", err)
	}

	// Main optimization loop
	for {
		o.state.Iteration++

		if maxIterations > 0 && o.state.Iteration > maxIterations {
			fmt.Printf("\n=== Reached max iterations (%d) ===\n", maxIterations)
			break
		}

		fmt.Printf("\n=== Iteration %d ===\n", o.state.Iteration)

		// Get proposal from LLM
		proposal, done, err := o.getProposal(context)
		if err != nil {
			return fmt.Errorf("failed to get proposal: %w", err)
		}

		if done {
			fmt.Println("\n✓ LLM indicates no more optimizations available")
			break
		}

		if proposal == nil {
			fmt.Println("No proposal returned, stopping")
			break
		}

		fmt.Printf("\nHypothesis: %s\n", proposal.Hypothesis)
		fmt.Printf("Change: %s\n", proposal.Change)
		fmt.Printf("File: %s\n", proposal.File)

		if o.dryRun {
			fmt.Println("\n[DRY RUN] Would apply:")
			fmt.Printf("--- %s\n", proposal.File)
			fmt.Printf("+++ %s\n", proposal.File)
			fmt.Println(formatDiff(proposal.OldCode, proposal.NewCode))
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, "", o.state.CurrentP95, 0, false)
			continue
		}

		// Apply the change
		fmt.Println("\nApplying change...")
		originalContent, err := o.applyChange(proposal)
		if err != nil {
			fmt.Printf("Failed to apply change: %v\n", err)
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, "", o.state.CurrentP95, 0, false)
			continue
		}

		// Benchmark after change
		fmt.Println("Benchmarking...")
		afterStats, err := o.runBenchmark()
		if err != nil {
			fmt.Printf("Benchmark failed: %v, reverting\n", err)
			o.revertChange(proposal.File, originalContent)
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, "", o.state.CurrentP95, 0, false)
			continue
		}

		p95Before := o.state.CurrentP95
		p95After := afterStats.P95Ms
		improved := p95After < p95Before

		if improved {
			fmt.Printf("✓ Improved! p95: %.1fms → %.1fms (%.1fms faster)\n", p95Before, p95After, p95Before-p95After)
			diff := formatDiff(proposal.OldCode, proposal.NewCode)
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, diff, p95Before, p95After, true)

			// Update context with new code
			context, _ = o.gatherContext()
		} else {
			fmt.Printf("✗ No improvement. p95: %.1fms → %.1fms, reverting\n", p95Before, p95After)
			o.revertChange(proposal.File, originalContent)
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, "", p95Before, p95After, false)
		}

		// Check if target met
		if o.endpoint.Target.Duration() > 0 && o.state.CurrentP95 <= float64(o.endpoint.Target.Duration().Milliseconds()) {
			fmt.Printf("\n✓ Target met! p95 (%.1fms) is under target (%dms)\n",
				o.state.CurrentP95, o.endpoint.Target.Duration().Milliseconds())
			break
		}
	}

	// Print final summary
	o.printSummary()
	return nil
}

func (o *Optimizer) runBenchmark() (*benchmark.Stats, error) {
	opts := benchmark.Options{
		Requests:    benchmark.DefaultRequestsForMethod(o.endpoint.Method),
		Concurrency: 1,
		Delay:       50 * time.Millisecond,
	}
	return benchmark.Run(o.name, o.endpoint, opts)
}

func (o *Optimizer) gatherContext() (string, error) {
	// Use tools to gather context about the codebase
	var context strings.Builder

	context.WriteString("## Project Structure\n\n")

	// List top-level files
	result := tools.ExecuteTool(tools.ToolUse{
		Name:  "list_files",
		Input: map[string]interface{}{"path": "."},
	})
	context.WriteString("```\n")
	context.WriteString(result.Content)
	context.WriteString("\n```\n\n")

	// Try to find route definitions
	context.WriteString("## Route Definitions (grep results)\n\n")

	// Extract path from URL for searching
	urlPath := extractPath(o.endpoint.URL)
	if urlPath != "" {
		result = tools.ExecuteTool(tools.ToolUse{
			Name: "grep",
			Input: map[string]interface{}{
				"pattern": urlPath,
				"include": "*.go",
			},
		})
		if !result.IsError && result.Content != "No matches found" {
			context.WriteString("```\n")
			context.WriteString(result.Content)
			context.WriteString("\n```\n\n")
		}
	}

	return context.String(), nil
}

func (o *Optimizer) getProposal(context string) (*Proposal, bool, error) {
	systemPrompt := o.buildSystemPrompt()
	userPrompt := o.buildUserPrompt(context)

	// For proposals, we want structured JSON output, not tool use
	// So we use a simpler request
	response, err := o.client.Complete(systemPrompt, userPrompt)
	if err != nil {
		return nil, false, err
	}

	// Parse the JSON response
	var proposalResp ProposalResponse
	if err := json.Unmarshal([]byte(response), &proposalResp); err != nil {
		// Try to extract JSON from the response
		jsonStr := extractJSON(response)
		if jsonStr != "" {
			if err := json.Unmarshal([]byte(jsonStr), &proposalResp); err != nil {
				return nil, false, fmt.Errorf("failed to parse proposal: %w\nResponse: %s", err, response)
			}
		} else {
			return nil, false, fmt.Errorf("failed to parse proposal: %w\nResponse: %s", err, response)
		}
	}

	return proposalResp.Proposal, proposalResp.Done, nil
}

func (o *Optimizer) buildSystemPrompt() string {
	prompt := `You are autoprobe, an AI performance optimizer. Your job is to propose ONE specific code change to improve API endpoint performance.

IMPORTANT: You must respond with ONLY valid JSON in this exact format:

{
  "proposal": {
    "hypothesis": "Brief explanation of what you think is slow and why",
    "change": "Human-readable description of the change",
    "file": "path/to/file.go",
    "old_code": "exact code to find and replace",
    "new_code": "the optimized replacement code"
  }
}

Or if you believe no more optimizations are possible:

{
  "done": true,
  "done_reason": "Explanation of why no more optimizations are possible"
}

Rules:
- Propose only ONE change at a time
- The old_code must match EXACTLY what's in the file (including whitespace)
- Focus on high-impact changes: N+1 queries, missing indexes, inefficient algorithms
- Do NOT propose changes that were already tried and failed`

	if o.cfg.Rules != "" {
		prompt += "\n\nAdditional rules from user:\n" + o.cfg.Rules
	}

	return prompt
}

func (o *Optimizer) buildUserPrompt(context string) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Target Endpoint\n\n"))
	sb.WriteString(fmt.Sprintf("- Name: %s\n", o.name))
	sb.WriteString(fmt.Sprintf("- Method: %s\n", o.endpoint.Method))
	sb.WriteString(fmt.Sprintf("- URL: %s\n", o.endpoint.URL))
	if o.endpoint.Target.Duration() > 0 {
		sb.WriteString(fmt.Sprintf("- Target: %s\n", o.endpoint.Target.Duration()))
	}
	sb.WriteString("\n")

	sb.WriteString(o.state.FormatSummary())
	sb.WriteString("\n")
	sb.WriteString(o.state.FormatHistory())
	sb.WriteString("\n")
	sb.WriteString(context)

	sb.WriteString("\nAnalyze the code and propose ONE optimization. Use grep/read_file patterns if you need to see more code before proposing.")

	return sb.String()
}

func (o *Optimizer) applyChange(proposal *Proposal) (string, error) {
	// Read original content
	content, err := os.ReadFile(proposal.File)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	originalContent := string(content)

	// Check if old_code exists
	if !strings.Contains(originalContent, proposal.OldCode) {
		return "", fmt.Errorf("old_code not found in file")
	}

	// Count occurrences
	count := strings.Count(originalContent, proposal.OldCode)
	if count > 1 {
		return "", fmt.Errorf("old_code found %d times, need exactly 1", count)
	}

	// Apply the change
	newContent := strings.Replace(originalContent, proposal.OldCode, proposal.NewCode, 1)

	if err := os.WriteFile(proposal.File, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return originalContent, nil
}

func (o *Optimizer) revertChange(file, originalContent string) {
	os.WriteFile(file, []byte(originalContent), 0644)
}

func (o *Optimizer) printSummary() {
	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("OPTIMIZATION SUMMARY")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("\nEndpoint: %s\n", o.name)
	fmt.Printf("Duration: %s\n", time.Since(o.state.StartTime).Round(time.Second))
	fmt.Printf("Iterations: %d\n", o.state.Iteration)
	fmt.Printf("\nPerformance:\n")
	fmt.Printf("  Baseline p95: %.1fms\n", o.state.BaselineP95)
	fmt.Printf("  Final p95:    %.1fms\n", o.state.CurrentP95)
	fmt.Printf("  Improvement:  %.1fms (%.1f%%)\n",
		o.state.TotalImprovement(),
		(o.state.TotalImprovement()/o.state.BaselineP95)*100)

	successful := o.state.SuccessfulAttempts()
	failed := o.state.FailedAttempts()

	if len(successful) > 0 {
		fmt.Printf("\nSuccessful changes (%d):\n", len(successful))
		for _, a := range successful {
			fmt.Printf("  ✓ %s (%.1fms faster)\n", a.Change, a.P95Before-a.P95After)
		}
	}

	if len(failed) > 0 {
		fmt.Printf("\nFailed attempts (%d):\n", len(failed))
		for _, a := range failed {
			fmt.Printf("  ✗ %s\n", a.Hypothesis)
		}
	}
}

// Helper functions

func extractPath(url string) string {
	// Extract the path portion from URL for grep
	// e.g., "http://localhost:8080/v1/apps" -> "/v1/apps"
	if idx := strings.Index(url, "://"); idx != -1 {
		rest := url[idx+3:]
		if idx := strings.Index(rest, "/"); idx != -1 {
			path := rest[idx:]
			// Remove query string
			if idx := strings.Index(path, "?"); idx != -1 {
				path = path[:idx]
			}
			return path
		}
	}
	return ""
}

func extractJSON(s string) string {
	// Try to find JSON object in the response
	start := strings.Index(s, "{")
	if start == -1 {
		return ""
	}

	depth := 0
	for i := start; i < len(s); i++ {
		if s[i] == '{' {
			depth++
		} else if s[i] == '}' {
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func formatDiff(old, new string) string {
	var sb strings.Builder
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")

	for _, line := range oldLines {
		sb.WriteString("- " + line + "\n")
	}
	for _, line := range newLines {
		sb.WriteString("+ " + line + "\n")
	}
	return sb.String()
}
