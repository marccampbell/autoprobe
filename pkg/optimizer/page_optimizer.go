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
	Name          string
	Iteration     int
	StartTime     time.Time
	BaselineStats *pagebench.PageStats
	CurrentStats  *pagebench.PageStats
	Attempts      []PageAttempt
}

// PageAttempt records a single optimization attempt
type PageAttempt struct {
	Hypothesis string
	Change     string
	File       string
	Diff       string
	Kept       bool
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
	// Initial benchmark (3 runs, use median)
	fmt.Println("\n=== Initial Page Benchmark (3 runs) ===")
	baseline, err := pagebench.RunMultiple(o.name, o.page, 3, o.verbose)
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

	// Pre-gather context once (expensive grep operations)
	fmt.Printf("\nGathering code context...")
	codeContext := o.gatherCodeContext(slowRequests)
	fmt.Println(" done")

	// Main optimization loop
	for {
		if maxIterations > 0 && o.state.Iteration >= maxIterations {
			fmt.Printf("\n=== Completed %d iterations ===\n", maxIterations)
			break
		}

		o.state.Iteration++
		fmt.Printf("\n=== Iteration %d ===\n", o.state.Iteration)

		// Get proposal
		fmt.Printf("Analyzing...")
		proposal, done, err := o.getProposal(codeContext, slowRequests)
		fmt.Println()

		if err != nil {
			fmt.Printf("Failed: %v\n", err)
			if o.state.Iteration >= 3 {
				fmt.Println("Too many failures, stopping")
				break
			}
			o.state.Iteration--
			continue
		}

		if done {
			fmt.Println("No more optimizations identified.")
			break
		}

		if proposal == nil {
			fmt.Println("No proposal (retrying)")
			o.state.Iteration--
			continue
		}

		fmt.Printf("\nHypothesis: %s\n", proposal.Hypothesis)
		fmt.Printf("Change: %s\n", proposal.Change)
		fmt.Printf("File: %s\n", proposal.File)

		if o.dryRun {
			fmt.Println("\n[DRY RUN] Would apply:")
			fmt.Println(formatDiff(proposal.OldCode, proposal.NewCode))
			continue
		}

		// Apply the change
		fmt.Print("Applying... ")
		originalContent, err := o.applyChange(proposal)
		if err != nil {
			fmt.Printf("failed: %v\n", err)
			o.state.Iteration--
			continue
		}
		fmt.Println("done")

		// Re-benchmark (3 runs, use median)
		fmt.Print("Benchmarking (3 runs)... ")
		afterStats, err := pagebench.RunMultiple(o.name, o.page, 3, false)
		if err != nil {
			fmt.Printf("failed: %v\n", err)
			o.revertChange(proposal.File, originalContent)
			continue
		}

		improved, beforeMs, afterMs := o.compareXHRTimings(o.state.CurrentStats, afterStats)

		if improved {
			fmt.Printf("KEEP ✓ (%.0fms → %.0fms, -%.0f%%)\n", beforeMs, afterMs, (beforeMs-afterMs)/beforeMs*100)
			o.state.CurrentStats = afterStats
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis: proposal.Hypothesis,
				Change:     proposal.Change,
				File:       proposal.File,
				Diff:       formatDiff(proposal.OldCode, proposal.NewCode),
				Kept:       true,
			})
		} else {
			diff := afterMs - beforeMs
			sign := "+"
			if diff < 0 {
				sign = ""
			}
			fmt.Printf("DISCARD ✗ (%.0fms → %.0fms, %s%.0fms)\n", beforeMs, afterMs, sign, diff)
			o.revertChange(proposal.File, originalContent)
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis: proposal.Hypothesis,
				Change:     proposal.Change,
				File:       proposal.File,
				Kept:       false,
			})
		}
	}

	o.printSummary()
	return nil
}

func (o *PageOptimizer) getSlowestXHR(stats *pagebench.PageStats, n int) []pagebench.RequestInfo {
	var xhrRequests []pagebench.RequestInfo
	for _, req := range stats.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			if pagebench.IsDevToolingURL(req.URL) {
				continue
			}
			xhrRequests = append(xhrRequests, req)
		}
	}

	sort.Slice(xhrRequests, func(i, j int) bool {
		return xhrRequests[i].Duration > xhrRequests[j].Duration
	})

	if len(xhrRequests) > n {
		return xhrRequests[:n]
	}
	return xhrRequests
}

// gatherCodeContext pre-greps for relevant code to reduce exploration needed
func (o *PageOptimizer) gatherCodeContext(slowRequests []pagebench.RequestInfo) string {
	var ctx strings.Builder

	// Project structure (just top level)
	result := tools.ExecuteTool(tools.ToolUse{
		Name:  "list_files",
		Input: map[string]interface{}{"path": "."},
	})
	ctx.WriteString("## Project Structure\n```\n")
	ctx.WriteString(result.Content)
	ctx.WriteString("\n```\n\n")

	// Find code for each API path
	seenPaths := make(map[string]bool)
	for _, req := range slowRequests {
		path := extractAPIPath(req.URL)
		if path == "" || seenPaths[path] {
			continue
		}
		seenPaths[path] = true

		result := tools.ExecuteTool(tools.ToolUse{
			Name: "grep",
			Input: map[string]interface{}{
				"pattern": path,
				"include": "*.tsx,*.ts,*.jsx,*.js",
			},
		})

		if !result.IsError && result.Content != "No matches found" {
			content := result.Content
			if len(content) > 3000 {
				content = content[:3000] + "\n... (truncated)"
			}
			ctx.WriteString(fmt.Sprintf("## Code referencing %s\n```\n%s\n```\n\n", path, content))
		}
	}

	// Also grep for common patterns
	for _, pattern := range []string{"useEffect", "useFetch", "useQuery"} {
		result := tools.ExecuteTool(tools.ToolUse{
			Name: "grep",
			Input: map[string]interface{}{
				"pattern": pattern,
				"include": "*.tsx,*.jsx",
			},
		})
		if !result.IsError && result.Content != "No matches found" {
			content := result.Content
			if len(content) > 2000 {
				content = content[:2000] + "\n... (truncated)"
			}
			ctx.WriteString(fmt.Sprintf("## Files using %s\n```\n%s\n```\n\n", pattern, content))
		}
	}

	return ctx.String()
}

func (o *PageOptimizer) getProposal(codeContext string, slowRequests []pagebench.RequestInfo) (*Proposal, bool, error) {
	systemPrompt := `You are autoprobe, a frontend performance optimizer.

Your task: Find ONE client-side optimization to reduce slow/redundant XHR requests.

IMPORTANT: If there are previous attempts listed, you MUST propose something DIFFERENT.
- Don't modify the same file with a similar change
- Don't propose the inverse of a previous attempt
- Look for a completely different optimization opportunity

Common issues to look for:
1. Redundant API calls - same endpoint called multiple times
2. useEffect with missing/wrong deps causing re-fetches  
3. Missing React Query/SWR caching
4. Sequential requests that could be parallel
5. Components re-rendering and triggering unnecessary fetches

You have tools to read files and explore code. Use them efficiently:
- Start by reading the files shown in the context
- Look for useEffect, fetch, axios, or query patterns
- Identify the issue and propose a fix

When you find an optimization, output JSON:
{"proposal":{"hypothesis":"what's wrong","change":"how to fix it","file":"path/to/file.tsx","old_code":"exact code to replace","new_code":"fixed code"}}

If no NEW optimization found (you've tried everything reasonable):
{"done":true,"done_reason":"why"}

CRITICAL:
- old_code must be EXACT copy from the file (use read_file to get it)
- old_code must appear exactly ONCE in the file
- Focus on .tsx/.jsx/.ts/.js files only
- Don't optimize server-side code`

	var userPrompt strings.Builder
	
	// Add history of previous attempts (be very explicit to avoid repeats)
	if len(o.state.Attempts) > 0 {
		userPrompt.WriteString("## PREVIOUS ATTEMPTS - DO NOT REPEAT THESE\n")
		userPrompt.WriteString("You have already tried these optimizations. Find something DIFFERENT.\n\n")
		for i, a := range o.state.Attempts {
			status := "DISCARDED (didn't help)"
			if a.Kept {
				status = "KEPT"
			}
			userPrompt.WriteString(fmt.Sprintf("%d. [%s] File: %s\n   Hypothesis: %s\n   Change: %s\n\n", 
				i+1, status, a.File, a.Hypothesis, a.Change))
		}
	}

	// Add slow requests
	userPrompt.WriteString("## Slow XHR Requests\n")
	for _, req := range slowRequests {
		userPrompt.WriteString(fmt.Sprintf("- %s %s (%s)\n", req.Method, req.URL, req.Duration.Round(time.Millisecond)))
	}
	userPrompt.WriteString("\n")

	// Add pre-gathered code context
	userPrompt.WriteString(codeContext)

	userPrompt.WriteString("\nFind an optimization and output the JSON proposal.")

	// Run with tools
	availableTools := tools.GetTools(false)
	var response strings.Builder

	onMessage := func(text string) {
		response.WriteString(text)
		if o.verbose {
			fmt.Print(text)
		}
	}

	onToolUse := func(name string) {
		if o.verbose {
			fmt.Printf("\n[%s]\n", name)
		} else {
			fmt.Print(".")
		}
	}

	err := o.client.RunWithTools(systemPrompt, userPrompt.String(), availableTools, onMessage, onToolUse)
	if err != nil {
		return nil, false, err
	}

	// Extract JSON
	jsonStr := extractJSON(response.String())
	if jsonStr == "" {
		// Try asking for just the JSON
		var retry strings.Builder
		o.client.Complete(systemPrompt, response.String()+"\n\nNow output only the JSON proposal:", func(text string) {
			retry.WriteString(text)
		})
		jsonStr = extractJSON(retry.String())
	}

	if jsonStr == "" {
		last := response.String()
		if len(last) > 300 {
			last = last[len(last)-300:]
		}
		return nil, false, fmt.Errorf("no JSON found: ...%s", last)
	}

	jsonStr = fixJSONEscaping(jsonStr)

	var resp ProposalResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, false, fmt.Errorf("bad JSON: %w\n%s", err, jsonStr)
	}

	return resp.Proposal, resp.Done, nil
}

func (o *PageOptimizer) applyChange(proposal *Proposal) (string, error) {
	content, err := os.ReadFile(proposal.File)
	if err != nil {
		return "", fmt.Errorf("read failed: %w", err)
	}
	original := string(content)

	if !strings.Contains(original, proposal.OldCode) {
		return "", fmt.Errorf("old_code not found in file")
	}

	count := strings.Count(original, proposal.OldCode)
	if count > 1 {
		return "", fmt.Errorf("old_code found %d times (need 1)", count)
	}

	newContent := strings.Replace(original, proposal.OldCode, proposal.NewCode, 1)
	if err := os.WriteFile(proposal.File, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("write failed: %w", err)
	}

	return original, nil
}

func (o *PageOptimizer) revertChange(file, originalContent string) {
	os.WriteFile(file, []byte(originalContent), 0644)
}

func (o *PageOptimizer) compareXHRTimings(before, after *pagebench.PageStats) (bool, float64, float64) {
	beforeTotal := time.Duration(0)
	afterTotal := time.Duration(0)
	beforeXHRCount := 0
	afterXHRCount := 0

	for _, req := range before.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			beforeTotal += req.Duration
			beforeXHRCount++
		}
	}

	for _, req := range after.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			afterTotal += req.Duration
			afterXHRCount++
		}
	}

	beforeMs := float64(beforeTotal.Milliseconds())
	afterMs := float64(afterTotal.Milliseconds())
	
	// Count redundant (identical) XHR requests
	beforeRedundant := 0
	afterRedundant := 0
	for _, dup := range before.RedundantXHR {
		if dup.Identical {
			beforeRedundant += dup.Count - 1 // -1 because first request is needed
		}
	}
	for _, dup := range after.RedundantXHR {
		if dup.Identical {
			afterRedundant += dup.Count - 1
		}
	}
	
	// Win conditions:
	// 1. Timing improved by >5% without getting significantly slower
	// 2. Eliminated redundant identical requests (even if timing is similar)
	// 3. Reduced total XHR count without slowing down significantly
	
	timingImproved := (beforeMs-afterMs)/beforeMs > 0.05
	notSlowerThan10Pct := afterMs <= beforeMs*1.10
	reducedRedundant := afterRedundant < beforeRedundant && notSlowerThan10Pct
	reducedRequests := afterXHRCount < beforeXHRCount && notSlowerThan10Pct
	
	return timingImproved || reducedRedundant || reducedRequests, beforeMs, afterMs
}

func (o *PageOptimizer) printSummary() {
	fmt.Println("\n" + strings.Repeat("=", 50))
	fmt.Println("PAGE OPTIMIZATION SUMMARY")
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("\nPage: %s\n", o.name)
	fmt.Printf("Duration: %s\n", time.Since(o.state.StartTime).Round(time.Second))
	fmt.Printf("Iterations: %d\n", o.state.Iteration)

	kept := 0
	for _, a := range o.state.Attempts {
		if a.Kept {
			kept++
		}
	}

	fmt.Printf("\nAttempts: %d total (%d kept, %d discarded)\n", len(o.state.Attempts), kept, len(o.state.Attempts)-kept)

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
	if idx := strings.Index(url, "://"); idx != -1 {
		rest := url[idx+3:]
		if idx := strings.Index(rest, "/"); idx != -1 {
			path := rest[idx:]
			if idx := strings.Index(path, "?"); idx != -1 {
				path = path[:idx]
			}
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
