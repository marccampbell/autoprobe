package optimizer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/marccampbell/autoprobe/pkg/claude"
	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/marccampbell/autoprobe/pkg/pagebench"
	"github.com/marccampbell/autoprobe/pkg/tools"
)

// PageOptimizer runs the optimization loop for pages
type PageOptimizer struct {
	cfg     *config.Config
	page    *config.PageConfig
	name    string
	state   *PageRunState
	client  *claude.Client
	dryRun  bool
	verbose bool
}

// PageRunState tracks state for a page optimization run
type PageRunState struct {
	Name           string
	Iteration      int
	StartTime      time.Time
	BaselineStats  *pagebench.PageStats
	CurrentStats   *pagebench.PageStats
	Attempts       []PageAttempt
}

// PageAttempt records a single optimization attempt
type PageAttempt struct {
	Hypothesis     string
	Change         string
	File           string
	Diff           string
	TargetRequest  string  // Which XHR request we're trying to optimize
	BeforeMs       float64
	AfterMs        float64
	Kept           bool
}

// NewPageOptimizer creates a new page optimizer
func NewPageOptimizer(cfg *config.Config, pageName string, page *config.PageConfig, dryRun, verbose bool) (*PageOptimizer, error) {
	client, err := claude.NewClient()
	if err != nil {
		return nil, err
	}

	return &PageOptimizer{
		cfg:     cfg,
		page:    page,
		name:    pageName,
		client:  client,
		dryRun:  dryRun,
		verbose: verbose,
	}, nil
}

// Run executes the page optimization loop
func (o *PageOptimizer) Run(maxIterations int) error {
	// Initial benchmark
	fmt.Println("\n=== Initial Page Benchmark ===")
	baseline, err := pagebench.Run(o.name, o.page, o.verbose)
	if err != nil {
		return fmt.Errorf("initial benchmark failed: %w", err)
	}
	pagebench.PrintStats(baseline)

	// Initialize state
	o.state = &PageRunState{
		Name:          o.name,
		StartTime:     time.Now(),
		BaselineStats: baseline,
		CurrentStats:  baseline,
	}

	// Get slowest XHR requests
	slowRequests := o.getSlowestXHR(baseline, 5)
	if len(slowRequests) == 0 {
		fmt.Println("\nNo XHR/fetch requests found to optimize.")
		return nil
	}

	fmt.Printf("\nSlowest XHR requests to optimize:\n")
	for i, req := range slowRequests {
		fmt.Printf("  %d. %s (%s)\n", i+1, truncateURL(req.URL, 60), req.Duration.Round(time.Millisecond))
	}

	// Main optimization loop
	retries := 0
	maxRetries := 3
	for {
		if maxIterations > 0 && o.state.Iteration >= maxIterations {
			fmt.Printf("\n=== Reached max iterations (%d) ===\n", maxIterations)
			break
		}

		o.state.Iteration++
		fmt.Printf("\n=== Iteration %d ===\n", o.state.Iteration)

		// Gather context about the page and slow requests
		context := o.gatherContext(slowRequests)

		// Get proposal
		proposal, done, err := o.getProposalWithTools(context)
		if err != nil {
			fmt.Printf("Failed to get proposal: %v\n", err)
			retries++
			if retries >= maxRetries {
				fmt.Printf("Max retries (%d) reached, stopping\n", maxRetries)
				break
			}
			o.state.Iteration-- // Don't count failed attempts
			continue
		}
		retries = 0 // Reset on success

		if done {
			fmt.Println("No more optimizations identified.")
			break
		}

		if proposal == nil {
			fmt.Println("No proposal returned (retrying)")
			continue
		}

		fmt.Printf("Hypothesis: %s\n", proposal.Hypothesis)
		fmt.Printf("File: %s\n", proposal.File)

		if o.dryRun {
			fmt.Println("\n[DRY RUN] Would apply this change:")
			fmt.Println(formatDiff(proposal.OldCode, proposal.NewCode))
			continue
		}

		// Apply the change
		fmt.Println("\nApplying change...")
		originalContent, err := o.applyChange(proposal)
		if err != nil {
			fmt.Printf("Failed to apply: %v\n", err)
			continue
		}

		// Re-benchmark the page
		fmt.Println("Re-benchmarking page...")
		afterStats, err := pagebench.Run(o.name, o.page, false)
		if err != nil {
			fmt.Printf("Benchmark failed, reverting: %v\n", err)
			o.revertChange(proposal.File, originalContent)
			continue
		}

		// Compare XHR timings
		improved := o.compareXHRTimings(o.state.CurrentStats, afterStats)

		if improved {
			fmt.Printf("\nResult: KEEP — page XHR performance improved\n")
			o.state.CurrentStats = afterStats
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis: proposal.Hypothesis,
				Change:     proposal.Change,
				File:       proposal.File,
				Diff:       formatDiff(proposal.OldCode, proposal.NewCode),
				Kept:       true,
			})
		} else {
			fmt.Printf("\nResult: DISCARD — no improvement\n")
			o.revertChange(proposal.File, originalContent)
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis: proposal.Hypothesis,
				Change:     proposal.Change,
				File:       proposal.File,
				Kept:       false,
			})
		}
	}

	// Print final summary
	o.printSummary()
	return nil
}

func (o *PageOptimizer) getSlowestXHR(stats *pagebench.PageStats, n int) []pagebench.RequestInfo {
	var xhrRequests []pagebench.RequestInfo
	for _, req := range stats.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			// Skip dev tooling URLs
			if pagebench.IsDevToolingURL(req.URL) {
				continue
			}
			xhrRequests = append(xhrRequests, req)
		}
	}

	// Sort by duration descending
	sort.Slice(xhrRequests, func(i, j int) bool {
		return xhrRequests[i].Duration > xhrRequests[j].Duration
	})

	if len(xhrRequests) > n {
		return xhrRequests[:n]
	}
	return xhrRequests
}

func (o *PageOptimizer) gatherContext(slowRequests []pagebench.RequestInfo) string {
	var context strings.Builder

	context.WriteString("## Page Information\n")
	context.WriteString(fmt.Sprintf("URL: %s\n\n", o.page.URL))

	context.WriteString("## Slow XHR Requests to Optimize\n")
	for _, req := range slowRequests {
		context.WriteString(fmt.Sprintf("- %s %s (%s)\n", req.Method, req.URL, req.Duration.Round(time.Millisecond)))
	}
	context.WriteString("\n")

	// Get file tree
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

	// Grep for API paths from slow requests
	for _, req := range slowRequests {
		path := extractAPIPath(req.URL)
		if path != "" {
			result = tools.ExecuteTool(tools.ToolUse{
				Name: "grep",
				Input: map[string]interface{}{
					"pattern": path,
					"include": "*.go,*.ts,*.tsx,*.js,*.jsx",
				},
			})
			if !result.IsError && result.Content != "No matches found" {
				grepResults := result.Content
				if len(grepResults) > 2000 {
					grepResults = grepResults[:2000] + "\n..."
				}
				context.WriteString(fmt.Sprintf("## Code references for %s\n```\n", path))
				context.WriteString(grepResults)
				context.WriteString("\n```\n\n")
			}
		}
	}

	return context.String()
}

func (o *PageOptimizer) getProposalWithTools(context string) (*Proposal, bool, error) {
	systemPrompt := `You are autoprobe, an AI performance optimizer for web pages.

Your task is to optimize page load performance by:
1. Analyzing slow XHR/fetch requests
2. Looking at BOTH client-side code (React, etc.) AND server-side code
3. Proposing optimizations to either side

Types of optimizations to consider:
- CLIENT: Reduce unnecessary API calls, batch requests, add caching, lazy load
- CLIENT: Optimize React rendering, reduce re-renders that trigger fetches
- SERVER: Optimize database queries, add indexes, reduce N+1 queries
- SERVER: Add caching, optimize serialization, reduce response size

OUTPUT FORMAT (must be valid JSON, nothing after):

{"proposal":{"hypothesis":"...","change":"...","file":"...","old_code":"exact code from file","new_code":"optimized code"}}

Or if no optimization found:

{"done":true,"done_reason":"..."}

RULES:
- old_code must match file EXACTLY (copy/paste from read_file output)
- ONE change only per proposal
- Can modify client OR server code
- No narration text, just hypothesis then tools then JSON`

	if o.cfg.Rules != "" {
		systemPrompt += "\n\nUser rules:\n" + o.cfg.Rules
	}

	var userPrompt strings.Builder
	userPrompt.WriteString(o.formatHistory())
	userPrompt.WriteString("\n")
	userPrompt.WriteString(context)
	userPrompt.WriteString("\nState your hypothesis, then investigate and propose a fix.")

	availableTools := tools.GetTools(false) // read-only

	var fullResponse strings.Builder
	var hypothesisPrinted bool
	
	onMessage := func(text string) {
		fullResponse.WriteString(text)
		
		if o.verbose {
			fmt.Print(text)
		} else {
			// Print the first substantive line as the hypothesis
			if !hypothesisPrinted {
				lines := strings.Split(text, "\n")
				for _, line := range lines {
					trimmed := strings.TrimSpace(line)
					// Skip empty lines and JSON
					if trimmed != "" && !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
						fmt.Println(trimmed)
						hypothesisPrinted = true
						break
					}
				}
			}
		}
	}
	
	var onToolUse func(string)
	if o.verbose {
		onToolUse = func(toolName string) {
			fmt.Printf("\n[TOOL: %s]\n", toolName)
		}
	}
	
	err := o.client.RunWithTools(systemPrompt, userPrompt.String(), availableTools, onMessage, onToolUse)
	
	if o.verbose {
		fmt.Println("\n--- END LLM OUTPUT ---")
	}

	if err != nil {
		return nil, false, err
	}

	// Extract JSON from response
	jsonStr := extractJSON(fullResponse.String())
	if jsonStr == "" {
		// Debug: show what we got
		response := fullResponse.String()
		if len(response) > 500 {
			response = response[len(response)-500:]
		}
		return nil, false, fmt.Errorf("no JSON proposal found. Last 500 chars of response:\n%s", response)
	}

	jsonStr = fixJSONEscaping(jsonStr)

	var proposalResp ProposalResponse
	if err := json.Unmarshal([]byte(jsonStr), &proposalResp); err != nil {
		return nil, false, fmt.Errorf("invalid JSON: %w", err)
	}

	return proposalResp.Proposal, proposalResp.Done, nil
}

func (o *PageOptimizer) formatHistory() string {
	if len(o.state.Attempts) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Previous Attempts\n")
	for _, a := range o.state.Attempts {
		status := "DISCARDED"
		if a.Kept {
			status = "KEPT"
		}
		sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", status, a.File, a.Hypothesis))
	}
	sb.WriteString("\n")
	return sb.String()
}

func (o *PageOptimizer) applyChange(proposal *Proposal) (string, error) {
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

func (o *PageOptimizer) revertChange(file, originalContent string) {
	os.WriteFile(file, []byte(originalContent), 0644)
}

func (o *PageOptimizer) compareXHRTimings(before, after *pagebench.PageStats) bool {
	// Calculate total XHR time for before and after
	beforeTotal := time.Duration(0)
	afterTotal := time.Duration(0)

	for _, req := range before.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			beforeTotal += req.Duration
		}
	}

	for _, req := range after.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			afterTotal += req.Duration
		}
	}

	// Consider improved if total XHR time decreased by at least 5%
	improvement := float64(beforeTotal-afterTotal) / float64(beforeTotal)
	return improvement > 0.05
}

func (o *PageOptimizer) printSummary() {
	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("PAGE OPTIMIZATION SUMMARY")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("\nPage: %s\n", o.name)
	fmt.Printf("Duration: %s\n", time.Since(o.state.StartTime).Round(time.Second))
	fmt.Printf("Iterations: %d\n", o.state.Iteration)

	kept := 0
	discarded := 0
	for _, a := range o.state.Attempts {
		if a.Kept {
			kept++
		} else {
			discarded++
		}
	}

	fmt.Printf("\nAttempts: %d total (%d kept, %d discarded)\n", len(o.state.Attempts), kept, discarded)

	if kept > 0 {
		fmt.Printf("\nKept:\n")
		for _, a := range o.state.Attempts {
			if a.Kept {
				fmt.Printf("  ✓ %s: %s\n", a.File, a.Hypothesis)
			}
		}
	}
}

func extractAPIPath(url string) string {
	// Extract path from URL like http://vendor-api.localhost:8000/v3/app/123/channels
	if idx := strings.Index(url, "://"); idx != -1 {
		rest := url[idx+3:]
		if idx := strings.Index(rest, "/"); idx != -1 {
			path := rest[idx:]
			// Remove query string
			if idx := strings.Index(path, "?"); idx != -1 {
				path = path[:idx]
			}
			// Get just the route pattern (e.g., /v3/app or /v1/team)
			parts := strings.Split(path, "/")
			if len(parts) >= 3 {
				return "/" + parts[1] + "/" + parts[2]
			}
			return path
		}
	}
	return ""
}

func truncateURL(url string, n int) string {
	if len(url) <= n {
		return url
	}
	return url[:n-3] + "..."
}
