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

		// Quick context gathering (fast)
		context := o.gatherQuickContext()

		// Get proposal with tool access (Claude explores as needed)
		proposal, done, err := o.getProposalWithTools(context)
		if err != nil {
			fmt.Printf("Failed to get proposal: %v (retrying)\n", err)
			// Don't break - continue to next iteration
			continue
		}

		if done {
			fmt.Println("No more optimizations identified.")
			break
		}

		if proposal == nil {
			fmt.Println("No proposal returned (retrying)")
			continue
		}

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

// gatherQuickContext does fast context gathering without LLM
func (o *Optimizer) gatherQuickContext() string {
	var context strings.Builder

	// Get file tree (top level only) - truncate if too long
	result := tools.ExecuteTool(tools.ToolUse{
		Name:  "list_files",
		Input: map[string]interface{}{"path": "."},
	})
	fileList := result.Content
	if len(fileList) > 2000 {
		fileList = fileList[:2000] + "\n..."
	}
	context.WriteString("## Project Structure\n```\n")
	context.WriteString(fileList)
	context.WriteString("\n```\n\n")

	// Grep for the URL path - limit results
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
			grepResults := result.Content
			if len(grepResults) > 3000 {
				grepResults = grepResults[:3000] + "\n..."
			}
			context.WriteString("## Route matches\n```\n")
			context.WriteString(grepResults)
			context.WriteString("\n```\n\n")
		}
	}

	return context.String()
}

func extractPath(url string) string {
	if idx := strings.Index(url, "://"); idx != -1 {
		rest := url[idx+3:]
		if idx := strings.Index(rest, "/"); idx != -1 {
			path := rest[idx:]
			if idx := strings.Index(path, "?"); idx != -1 {
				path = path[:idx]
			}
			return path
		}
	}
	return ""
}

// getProposalWithTools lets Claude explore and propose in one pass
func (o *Optimizer) getProposalWithTools(context string) (*Proposal, bool, error) {
	systemPrompt := `You are autoprobe, an AI performance optimizer.

Your task:
1. State your HYPOTHESIS in one line (e.g., "HYPOTHESIS: N+1 query in user loader")
2. Investigate silently using tools - do NOT narrate what you're doing
3. Output a JSON proposal

When ready, output EXACTLY this JSON (nothing after):

{"proposal":{"hypothesis":"...","change":"...","file":"...","old_code":"...","new_code":"..."}}

Or if done:

{"done":true,"done_reason":"..."}

RULES:
- Hypothesis should be ONE short line
- Do NOT narrate your investigation steps (no "Let me look at...", "Now I'll check...")
- old_code must match the file EXACTLY including whitespace
- Propose ONE change only
- Focus on: N+1 queries, missing indexes, inefficient loops`

	if o.cfg.Rules != "" {
		systemPrompt += "\n\nUser rules:\n" + o.cfg.Rules
	}

	var userPrompt strings.Builder
	userPrompt.WriteString(fmt.Sprintf("## Target\n- %s %s\n\n", o.endpoint.Method, o.endpoint.URL))
	userPrompt.WriteString(o.state.FormatSummary())
	userPrompt.WriteString("\n")
	userPrompt.WriteString(o.state.FormatHistory())
	userPrompt.WriteString("\n")
	userPrompt.WriteString(context)
	userPrompt.WriteString("\nState your hypothesis, then investigate and propose a fix.")

	availableTools := tools.GetTools(false) // read-only

	var fullResponse strings.Builder
	var hypothesisPrinted bool
	err := o.client.RunWithTools(systemPrompt, userPrompt.String(), availableTools, func(text string) {
		fullResponse.WriteString(text)
		// Only print the hypothesis line, nothing else
		if !hypothesisPrinted && strings.Contains(strings.ToUpper(text), "HYPOTHESIS") {
			// Extract just the hypothesis line
			lines := strings.Split(text, "\n")
			for _, line := range lines {
				if strings.Contains(strings.ToUpper(line), "HYPOTHESIS") {
					fmt.Println(strings.TrimSpace(line))
					hypothesisPrinted = true
					break
				}
			}
		}
	})

	if err != nil {
		return nil, false, err
	}

	// Extract JSON from the full response
	jsonStr := extractJSON(fullResponse.String())
	if jsonStr == "" {
		return nil, false, fmt.Errorf("no JSON proposal found")
	}

	// Claude sometimes outputs JSON with literal tabs/newlines in strings
	// which is invalid JSON - try to fix common issues
	jsonStr = fixJSONEscaping(jsonStr)

	var proposalResp ProposalResponse
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
	
	fmt.Printf("\nAttempts: %d total (%d kept, %d discarded)\n", len(o.state.Attempts), len(successful), len(failed))

	if len(successful) > 0 {
		fmt.Printf("\nKept:\n")
		for _, a := range successful {
			fmt.Printf("  ✓ %s (-%.1fms)\n", a.Change, a.P95Before-a.P95After)
		}
	}

	if len(failed) > 0 {
		fmt.Printf("\nDiscarded:\n")
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

// fixJSONEscaping fixes common JSON escaping issues from LLM output
func fixJSONEscaping(s string) string {
	// Replace literal tabs with escaped tabs
	// But only inside string values, not the JSON structure
	// This is a simple approach - find strings and fix them
	
	var result strings.Builder
	inString := false
	escaped := false
	
	for i := 0; i < len(s); i++ {
		c := s[i]
		
		if escaped {
			result.WriteByte(c)
			escaped = false
			continue
		}
		
		if c == '\\' && inString {
			escaped = true
			result.WriteByte(c)
			continue
		}
		
		if c == '"' {
			inString = !inString
			result.WriteByte(c)
			continue
		}
		
		if inString {
			// Replace problematic characters with escaped versions
			switch c {
			case '\t':
				result.WriteString("\\t")
			case '\n':
				result.WriteString("\\n")
			case '\r':
				result.WriteString("\\r")
			default:
				result.WriteByte(c)
			}
		} else {
			result.WriteByte(c)
		}
	}
	
	return result.String()
}
