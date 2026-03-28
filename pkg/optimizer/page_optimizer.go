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
	"github.com/marccampbell/autoprobe/pkg/fireworks"
	"github.com/marccampbell/autoprobe/pkg/groq"
	"github.com/marccampbell/autoprobe/pkg/pagebench"
	"github.com/marccampbell/autoprobe/pkg/tools"
)

// FastClient interface for exploration models (Groq, Fireworks)
type FastClient interface {
	RunWithTools(systemPrompt string, userPrompt string, availableTools []tools.Tool, onMessage func(string), onToolUse func(string)) error
}

// PageOptimizer runs the optimization loop for pages
type PageOptimizer struct {
	cfg        *config.Config
	page       *config.PageConfig
	name       string
	state      *PageRunState
	client     *claude.Client
	fastClient FastClient
	dryRun     bool
	verbose    bool
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

	// Fast client for exploration (Groq preferred, Fireworks fallback)
	var fastClient FastClient
	groqClient, groqErr := groq.NewClient()
	if groqErr == nil {
		fastClient = groqClient
	} else {
		fireworksClient, fwErr := fireworks.NewClient()
		if fwErr == nil {
			fastClient = fireworksClient
		} else {
			return nil, fmt.Errorf("page optimization requires GROQ_API_KEY or FIREWORKS_API_KEY")
		}
	}

	return &PageOptimizer{
		cfg:        cfg,
		page:       page,
		name:       pageName,
		fastClient: fastClient,
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
			fmt.Printf("\n=== Completed %d iterations ===\n", maxIterations)
			break
		}

		o.state.Iteration++
		fmt.Printf("\n=== Iteration %d ===\n", o.state.Iteration)

		// Gather context about the page and slow requests
		context := o.gatherContext(slowRequests)

		// Get proposal (Claude investigates and proposes)
		fmt.Printf("Analyzing code and forming hypothesis...")
		proposal, done, err := o.getProposalWithTools(context)
		fmt.Println() // newline after the status
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
			o.state.Iteration--
			continue
		}

		fmt.Printf("\nHypothesis: %s\n", proposal.Hypothesis)
		fmt.Printf("Change: %s\n", proposal.Change)
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
			o.state.Iteration-- // Don't count failed applies
			retries++
			if retries >= maxRetries {
				fmt.Printf("Max retries (%d) reached, stopping\n", maxRetries)
				break
			}
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
	// Phase 1: Fast exploration with Kimi K2.5 (if available)
	// Phase 2: Proposal generation with Claude
	
	explorationPrompt := `You are a code explorer. Your job is to investigate slow XHR requests and summarize findings.

TASK: Find the client-side code responsible for slow/redundant XHR requests.

Look for:
- React components making these API calls
- useEffect hooks triggering fetches
- Redux actions/thunks making requests
- Any obvious issues (redundant calls, missing deps, no caching)

Use tools to explore, then OUTPUT A SUMMARY of what you found:
- Which files contain the relevant code
- What patterns you see (redundant calls, waterfalls, etc.)
- Specific code snippets that could be optimized

Be concise. Focus on findings, not narration.`

	var userPrompt strings.Builder
	userPrompt.WriteString(o.formatHistory())
	userPrompt.WriteString("\n")
	userPrompt.WriteString(context)
	userPrompt.WriteString("\nInvestigate and summarize your findings.")

	availableTools := tools.GetTools(false) // read-only

	var explorationFindings strings.Builder
	
	onMessage := func(text string) {
		explorationFindings.WriteString(text)
		if o.verbose {
			fmt.Print(text)
		}
	}
	
	onToolUse := func(toolName string) {
		if o.verbose {
			fmt.Printf("\n[TOOL: %s]\n", toolName)
		} else {
			fmt.Print(".")
		}
	}

	// Use fast client (Kimi K2.5) for exploration
	var err error
	err = o.fastClient.RunWithTools(explorationPrompt, userPrompt.String(), availableTools, onMessage, onToolUse)
	
	if err != nil {
		return nil, false, err
	}

	// Phase 2: Generate proposal with Claude
	fmt.Print(" (generating proposal)")
	
	proposalPrompt := `You are autoprobe. Based on the code exploration findings below, propose ONE optimization.

OUTPUT (required JSON only, nothing else):
{"proposal":{"hypothesis":"one line explanation","change":"what the fix does","file":"path/to/file.tsx","old_code":"EXACT code from file","new_code":"fixed code"}}

OR if no optimization found:
{"done":true,"done_reason":"..."}

RULES:
- old_code must match the file EXACTLY (copy from the findings)
- old_code must be UNIQUE in the file (include enough context)
- ONE change only
- Focus on client-side .tsx/.jsx/.ts/.js files
- Do NOT optimize server/API code`

	if o.cfg.Rules != "" {
		proposalPrompt += "\n\nUser rules:\n" + o.cfg.Rules
	}

	proposalContext := fmt.Sprintf("## Exploration Findings\n%s\n\n## Slow Requests\n%s", 
		explorationFindings.String(), context)

	var proposalResponse strings.Builder
	err = o.client.Complete(proposalPrompt, proposalContext, func(text string) {
		proposalResponse.WriteString(text)
		if o.verbose {
			fmt.Print(text)
		}
	})
	
	if o.verbose {
		fmt.Println("\n--- END LLM OUTPUT ---")
	}

	if err != nil {
		return nil, false, err
	}

	// Extract JSON from response
	jsonStr := extractJSON(proposalResponse.String())
	
	// If no JSON found, ask Claude to try again
	if jsonStr == "" {
		fmt.Print(" (retrying)")
		
		followUp := `Output ONLY the JSON proposal. No explanation:
{"proposal":{"hypothesis":"...","change":"...","file":"...","old_code":"...","new_code":"..."}}
or {"done":true,"done_reason":"..."}`
		
		var jsonResponse strings.Builder
		err = o.client.Complete(proposalPrompt, proposalContext+"\n\n"+followUp, func(text string) {
			jsonResponse.WriteString(text)
		})
		if err != nil {
			return nil, false, err
		}
		
		jsonStr = extractJSON(jsonResponse.String())
	}
	
	if jsonStr == "" {
		response := proposalResponse.String()
		if len(response) > 500 {
			response = response[len(response)-500:]
		}
		return nil, false, fmt.Errorf("no JSON proposal found. Last 500 chars:\n%s", response)
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
