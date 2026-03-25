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

	// Main optimization loop
	for {
		o.state.Iteration++

		if maxIterations > 0 && o.state.Iteration > maxIterations {
			fmt.Printf("\n=== Reached max iterations (%d) ===\n", maxIterations)
			break
		}

		fmt.Printf("\n=== Iteration %d ===\n", o.state.Iteration)

		// Phase 1: Explore and analyze
		fmt.Println("Analyzing...")
		analysis, err := o.analyzeEndpoint()
		if err != nil {
			fmt.Printf("Analysis failed: %v\n", err)
			break
		}

		// Phase 2: Get proposal based on analysis
		fmt.Println("Generating proposal...")
		proposal, done, err := o.getProposal(analysis)
		if err != nil {
			fmt.Printf("Failed to get proposal: %v\n", err)
			break
		}

		if done {
			fmt.Println("No more optimizations identified.")
			break
		}

		if proposal == nil {
			fmt.Println("No proposal returned.")
			break
		}

		fmt.Printf("\nHypothesis: %s\n", proposal.Hypothesis)
		fmt.Printf("Change: %s\n", proposal.Change)
		fmt.Printf("File: %s\n", proposal.File)

		if o.dryRun {
			fmt.Println("\n[DRY RUN] Would apply this change:")
			fmt.Println(formatDiff(proposal.OldCode, proposal.NewCode))
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, "", o.state.CurrentP95, 0, false)
			continue
		}

		// Apply the change
		fmt.Println("\nApplying change...")
		originalContent, err := o.applyChange(proposal)
		if err != nil {
			fmt.Printf("Failed to apply: %v\n", err)
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, "", o.state.CurrentP95, 0, false)
			continue
		}

		// Benchmark after change
		fmt.Println("Benchmarking...")
		afterStats, err := o.runBenchmark()
		if err != nil {
			fmt.Printf("Benchmark failed, reverting: %v\n", err)
			o.revertChange(proposal.File, originalContent)
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, "", o.state.CurrentP95, 0, false)
			continue
		}

		p95Before := o.state.CurrentP95
		p95After := afterStats.P95Ms
		improved := p95After < p95Before

		if improved {
			improvement := p95Before - p95After
			fmt.Printf("\nResult: KEEP — p95 improved by %.1fms (%.1fms → %.1fms)\n", improvement, p95Before, p95After)
			diff := formatDiff(proposal.OldCode, proposal.NewCode)
			o.state.RecordAttempt(proposal.Hypothesis, proposal.Change, proposal.File, diff, p95Before, p95After, true)
		} else {
			fmt.Printf("\nResult: DISCARD — no improvement (%.1fms → %.1fms)\n", p95Before, p95After)
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

// analyzeEndpoint uses tools to explore the codebase and returns analysis text
func (o *Optimizer) analyzeEndpoint() (string, error) {
	systemPrompt := `You are a code analyzer. Your job is to explore a codebase to understand an API endpoint's implementation.

Use the provided tools to:
1. Find the route/handler for the endpoint
2. Read the handler code
3. Trace calls to services, repositories, database queries
4. Look for performance issues (N+1 queries, missing indexes, inefficient loops)

After exploring, provide a brief summary of what you found and potential optimizations.

IMPORTANT: 
- Do NOT use git commands
- Do NOT modify any files
- Only read and search`

	userPrompt := fmt.Sprintf(`Analyze this endpoint:
- Method: %s
- URL: %s

Find the handler code and trace the implementation. Look for performance bottlenecks.
`, o.endpoint.Method, o.endpoint.URL)

	// Use read-only tools for analysis
	availableTools := tools.GetTools(false)

	var analysis strings.Builder
	err := o.client.RunWithTools(systemPrompt, userPrompt, availableTools, func(text string) {
		analysis.WriteString(text)
	})

	return analysis.String(), err
}

// getProposal asks for a structured optimization proposal
func (o *Optimizer) getProposal(analysis string) (*Proposal, bool, error) {
	systemPrompt := `You are a code optimizer. Based on the analysis provided, propose ONE specific code change.

You MUST respond with ONLY valid JSON in this exact format:

{
  "proposal": {
    "hypothesis": "Brief explanation of what is slow and why",
    "change": "Human-readable description of the fix",
    "file": "path/to/file.go",
    "old_code": "exact code to replace (must match file exactly)",
    "new_code": "the optimized replacement"
  }
}

Or if no optimizations are possible:

{
  "done": true,
  "done_reason": "Why no more optimizations are possible"
}

Rules:
- Propose ONE change only
- old_code must match EXACTLY (including whitespace)
- Focus on high-impact: N+1 queries, missing indexes, inefficient algorithms
- Do NOT propose changes already tried`

	if o.cfg.Rules != "" {
		systemPrompt += "\n\nUser rules:\n" + o.cfg.Rules
	}

	var userPrompt strings.Builder
	userPrompt.WriteString("## Analysis Results\n\n")
	userPrompt.WriteString(analysis)
	userPrompt.WriteString("\n\n")
	userPrompt.WriteString(o.state.FormatSummary())
	userPrompt.WriteString("\n")
	userPrompt.WriteString(o.state.FormatHistory())
	userPrompt.WriteString("\n\nBased on this analysis, propose ONE optimization. Respond with JSON only.")

	response, err := o.client.Complete(systemPrompt, userPrompt.String())
	if err != nil {
		return nil, false, err
	}

	// Parse JSON response
	var proposalResp ProposalResponse
	jsonStr := extractJSON(response)
	if jsonStr == "" {
		return nil, false, fmt.Errorf("no JSON found in response")
	}

	if err := json.Unmarshal([]byte(jsonStr), &proposalResp); err != nil {
		return nil, false, fmt.Errorf("invalid JSON: %w", err)
	}

	return proposalResp.Proposal, proposalResp.Done, nil
}

func (o *Optimizer) applyChange(proposal *Proposal) (string, error) {
	content, err := os.ReadFile(proposal.File)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}
	originalContent := string(content)

	if !strings.Contains(originalContent, proposal.OldCode) {
		return "", fmt.Errorf("old_code not found in file")
	}

	count := strings.Count(originalContent, proposal.OldCode)
	if count > 1 {
		return "", fmt.Errorf("old_code found %d times, need exactly 1", count)
	}

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
	fmt.Printf("\nBaseline p95: %.1fms\n", o.state.BaselineP95)
	fmt.Printf("Final p95:    %.1fms\n", o.state.CurrentP95)

	improvement := o.state.TotalImprovement()
	if improvement > 0 {
		fmt.Printf("Improvement:  %.1fms (%.1f%% faster)\n", improvement, (improvement/o.state.BaselineP95)*100)
	}

	successful := o.state.SuccessfulAttempts()
	failed := o.state.FailedAttempts()

	if len(successful) > 0 {
		fmt.Printf("\nKept (%d):\n", len(successful))
		for _, a := range successful {
			fmt.Printf("  ✓ %s (-%.1fms)\n", a.Change, a.P95Before-a.P95After)
		}
	}

	if len(failed) > 0 {
		fmt.Printf("\nDiscarded (%d):\n", len(failed))
		for _, a := range failed {
			fmt.Printf("  ✗ %s\n", a.Hypothesis)
		}
	}
}

// Helper functions

func extractJSON(s string) string {
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
	for _, line := range strings.Split(old, "\n") {
		sb.WriteString("- " + line + "\n")
	}
	for _, line := range strings.Split(new, "\n") {
		sb.WriteString("+ " + line + "\n")
	}
	return sb.String()
}
