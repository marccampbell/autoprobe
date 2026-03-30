package optimizer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	client     *claude.Client   // For proposals
	fastClient FastClient       // For exploration
	dryRun     bool
	verbose    bool
	repoRoot   string
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
	Hypothesis   string
	Change       string
	FilesChanged []string
	Kept         bool
}

// NewPageOptimizer creates a new page optimizer
func NewPageOptimizer(cfg *config.Config, pageName string, page *config.PageConfig, dryRun, verbose bool) (*PageOptimizer, error) {
	// Find repo root
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("not in a git repository: %w", err)
	}
	repoRoot := strings.TrimSpace(string(output))

	// Claude client for proposals
	claudeClient, err := claude.NewClient()
	if err != nil {
		return nil, err
	}

	// Fast client for exploration (Groq preferred for speed, Fireworks fallback)
	var fastClient FastClient
	if groqClient, err := groq.NewClient(); err == nil {
		fastClient = groqClient
	} else if fwClient, err := fireworks.NewClient(); err == nil {
		fastClient = fwClient
	} else {
		return nil, fmt.Errorf("page optimization requires GROQ_API_KEY or FIREWORKS_API_KEY for fast exploration")
	}

	return &PageOptimizer{
		cfg:        cfg,
		page:       page,
		name:       pageName,
		client:     claudeClient,
		fastClient: fastClient,
		dryRun:     dryRun,
		verbose:    verbose,
		repoRoot:   repoRoot,
	}, nil
}

// Run executes the page optimization loop
func (o *PageOptimizer) Run(maxIterations int) error {
	// Initial benchmark
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
		fmt.Printf("  %d. %s (%s)\n", i+1, truncateURLMiddle(req.URL, 60), req.Duration.Round(time.Millisecond))
	}

	// Pre-gather context
	fmt.Print("\nGathering code context...")
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

		// Phase 1: Fast exploration with Groq/Fireworks
		fmt.Print("Exploring codebase...")
		findings, err := o.explore(codeContext, slowRequests)
		if err != nil {
			fmt.Printf(" failed: %v\n", err)
			continue
		}
		fmt.Println(" done")

		// Phase 2: Generate hypothesis with Claude API
		fmt.Print("Generating hypothesis...")
		hypothesis, change, err := o.generateHypothesis(findings, slowRequests)
		if err != nil {
			fmt.Printf(" failed: %v\n", err)
			continue
		}
		if hypothesis == "" {
			fmt.Println(" no more optimizations found")
			break
		}
		fmt.Println(" done")

		fmt.Printf("\nHypothesis: %s\n", hypothesis)
		fmt.Printf("Change: %s\n", change)

		if o.dryRun {
			fmt.Println("\n[DRY RUN] Would create worktree and run Claude CLI")
			continue
		}

		// Phase 3: Create worktree and run Claude CLI to make changes
		worktreePath, branchName, err := o.createWorktree()
		if err != nil {
			fmt.Printf("Failed to create worktree: %v\n", err)
			continue
		}

		fmt.Print("Applying changes with Claude CLI...")
		err = o.runClaudeCLI(worktreePath, hypothesis, change)
		if err != nil {
			fmt.Printf(" failed: %v\n", err)
			o.cleanupWorktree(worktreePath, branchName)
			continue
		}
		fmt.Println(" done")

		// Commit changes in worktree
		commitHash, changedFiles, err := o.commitWorktreeChanges(worktreePath)
		if err != nil || len(changedFiles) == 0 {
			fmt.Println("No changes made")
			o.cleanupWorktree(worktreePath, branchName)
			continue
		}
		fmt.Printf("Changed: %s\n", strings.Join(changedFiles, ", "))

		// Cherry-pick to main repo for benchmarking
		fmt.Print("Cherry-picking to main... ")
		err = o.cherryPick(commitHash)
		if err != nil {
			fmt.Printf("failed: %v\n", err)
			o.cleanupWorktree(worktreePath, branchName)
			continue
		}
		fmt.Println("done")

		// Benchmark
		fmt.Print("Benchmarking (3 runs)... ")
		afterStats, err := pagebench.RunMultiple(o.name, o.page, 3, false)
		if err != nil {
			fmt.Printf("failed: %v\n", err)
			o.revertCherryPick()
			o.cleanupWorktree(worktreePath, branchName)
			continue
		}

		improved, beforeMs, afterMs, beforeCount, afterCount := o.compareXHRTimings(o.state.CurrentStats, afterStats)

		// Check for new console errors (regression)
		newErrors := pagebench.FindNewConsoleErrors(o.state.CurrentStats, afterStats)
		if len(newErrors) > 0 {
			fmt.Printf("DISCARD ✗ — new console errors:\n")
			for _, e := range newErrors {
				if len(e) > 100 {
					e = e[:100] + "..."
				}
				fmt.Printf("  • %s\n", e)
			}
			o.revertCherryPick()
			o.cleanupWorktree(worktreePath, branchName)
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis:   hypothesis,
				Change:       change,
				FilesChanged: changedFiles,
				Kept:         false,
			})
			continue
		}

		// Check visual regression
		similarity, err := pagebench.CompareScreenshots(o.state.CurrentStats.ScreenshotPath, afterStats.ScreenshotPath)
		if err == nil && similarity < 0.85 {
			fmt.Printf("DISCARD ✗ — visual regression (%.0f%% similar, need 85%%+)\n", similarity*100)
			o.revertCherryPick()
			o.cleanupWorktree(worktreePath, branchName)
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis:   hypothesis,
				Change:       change,
				FilesChanged: changedFiles,
				Kept:         false,
			})
			continue
		}

		if improved {
			fmt.Printf("KEEP ✓ (%.0fms → %.0fms, %d → %d reqs)\n", beforeMs, afterMs, beforeCount, afterCount)
			
			// Auto-commit the changes
			commitMsg := fmt.Sprintf("perf: %s", hypothesis)
			if len(commitMsg) > 72 {
				commitMsg = commitMsg[:69] + "..."
			}
			fmt.Print("Committing... ")
			commitHash, err := o.commitChanges(commitMsg)
			if err != nil {
				fmt.Printf("failed: %v\n", err)
			} else {
				fmt.Printf("done (%s)\n", commitHash[:7])
			}
			
			o.state.CurrentStats = afterStats
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis:   hypothesis,
				Change:       change,
				FilesChanged: changedFiles,
				Kept:         true,
			})
		} else {
			diff := afterMs - beforeMs
			sign := "+"
			if diff < 0 {
				sign = ""
			}
			fmt.Printf("DISCARD ✗ (%.0fms → %.0fms, %s%.0fms, %d → %d reqs)\n", beforeMs, afterMs, sign, diff, beforeCount, afterCount)
			o.revertCherryPick()
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				Hypothesis:   hypothesis,
				Change:       change,
				FilesChanged: changedFiles,
				Kept:         false,
			})
		}

		o.cleanupWorktree(worktreePath, branchName)
	}

	o.printSummary()
	return nil
}

func (o *PageOptimizer) gatherCodeContext(slowRequests []pagebench.RequestInfo) string {
	var ctx strings.Builder

	// Project structure
	result := tools.ExecuteTool(tools.ToolUse{
		Name:  "list_files",
		Input: map[string]interface{}{"path": "."},
	})
	ctx.WriteString("## Project Structure\n```\n")
	ctx.WriteString(result.Content)
	ctx.WriteString("\n```\n\n")

	// Grep for API paths
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

	return ctx.String()
}

func (o *PageOptimizer) explore(codeContext string, slowRequests []pagebench.RequestInfo) (string, error) {
	prompt := `You are a code explorer. You MUST use tools to investigate. Do not explain what you will do - just call the tools.

AVAILABLE TOOLS:
- grep: {"pattern": "text", "include": "*.tsx"}
- read_file: {"path": "file.tsx"}
- list_files: {"path": "directory"}
- repo_browser.print_tree: {"path": "directory", "depth": 2}

TASK: Find client-side code causing redundant XHR requests.

START IMMEDIATELY by calling grep or read_file. Do not output any text before using a tool.`

	var userPrompt strings.Builder
	if len(o.state.Attempts) > 0 {
		userPrompt.WriteString("## Previous Attempts (find something DIFFERENT)\n")
		for _, a := range o.state.Attempts {
			status := "DISCARDED"
			if a.Kept {
				status = "KEPT"
			}
			userPrompt.WriteString(fmt.Sprintf("- [%s] %s\n", status, a.Hypothesis))
		}
		userPrompt.WriteString("\n")
	}

	// Add redundant XHR info - this is critical low-hanging fruit
	if len(o.state.CurrentStats.RedundantXHR) > 0 {
		userPrompt.WriteString("## 🔴 REDUNDANT XHR (DUPLICATE REQUESTS - FIX THESE FIRST)\n")
		for _, dup := range o.state.CurrentStats.RedundantXHR {
			if pagebench.IsDevToolingURL(dup.URL) {
				continue
			}
			userPrompt.WriteString(fmt.Sprintf("- %dx %s [%dms wasted]\n", dup.Count, dup.URL, dup.TotalTimeMs))
		}
		userPrompt.WriteString("\n")
	}

	userPrompt.WriteString("## Slow Requests\n")
	for _, req := range slowRequests {
		userPrompt.WriteString(fmt.Sprintf("- %s %s (%s)\n", req.Method, req.URL, req.Duration.Round(time.Millisecond)))
	}
	userPrompt.WriteString("\n")
	userPrompt.WriteString(codeContext)

	availableTools := tools.GetTools(false)
	var findings strings.Builder

	onMessage := func(text string) {
		findings.WriteString(text)
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

	err := o.fastClient.RunWithTools(prompt, userPrompt.String(), availableTools, onMessage, onToolUse)
	if err != nil {
		return "", err
	}

	return findings.String(), nil
}

func (o *PageOptimizer) generateHypothesis(findings string, slowRequests []pagebench.RequestInfo) (string, string, error) {
	prompt := `Based on the exploration findings, propose ONE optimization.

PRIORITIZE CLIENT-SIDE (try these first):
- Remove duplicate API calls in components (check RedundantXHR list!)
- Fix useEffect dependencies to prevent re-fetches
- Increase staleTime/cacheTime in React Query
- Batch requests or add deduplication
- Memoize components to prevent re-render fetches

IF CLIENT-SIDE IS ALREADY GOOD, then consider:
- Combining multiple API endpoints into one
- Reducing API response payload size

DO NOT suggest:
- Database indexes or query changes

Output JSON only:
{"hypothesis": "what causes the problem", "change": "what code to modify"}

Or if no optimization found:
{"done": true}`

	var context strings.Builder
	
	// Add redundant XHR info prominently
	if len(o.state.CurrentStats.RedundantXHR) > 0 {
		context.WriteString("## 🔴 REDUNDANT XHR (DUPLICATE REQUESTS - FIX THESE FIRST)\n")
		for _, dup := range o.state.CurrentStats.RedundantXHR {
			if pagebench.IsDevToolingURL(dup.URL) {
				continue
			}
			context.WriteString(fmt.Sprintf("- %dx %s [%dms wasted]\n", dup.Count, dup.URL, dup.TotalTimeMs))
		}
		context.WriteString("\n")
	}
	
	context.WriteString(fmt.Sprintf("## Exploration Findings\n%s\n\n## Slow Requests\n", findings))
	for _, req := range slowRequests {
		context.WriteString(fmt.Sprintf("- %s %s (%s)\n", req.Method, req.URL, req.Duration.Round(time.Millisecond)))
	}

	var response strings.Builder
	err := o.client.Complete(prompt, context.String(), func(text string) {
		response.WriteString(text)
	})
	if err != nil {
		return "", "", err
	}

	// Parse JSON
	jsonStr := extractJSON(response.String())
	if jsonStr == "" {
		return "", "", fmt.Errorf("no JSON in response")
	}

	var result struct {
		Hypothesis string `json:"hypothesis"`
		Change     string `json:"change"`
		Done       bool   `json:"done"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return "", "", err
	}

	if result.Done {
		return "", "", nil
	}

	return result.Hypothesis, result.Change, nil
}

func (o *PageOptimizer) createWorktree() (string, string, error) {
	branchName := fmt.Sprintf("autoprobe-exp-%d", time.Now().UnixNano())
	worktreePath := filepath.Join(os.TempDir(), branchName)

	cmd := exec.Command("git", "worktree", "add", "-b", branchName, worktreePath, "HEAD")
	cmd.Dir = o.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("git worktree add failed: %w\n%s", err, output)
	}

	return worktreePath, branchName, nil
}

func (o *PageOptimizer) cleanupWorktree(worktreePath, branchName string) {
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = o.repoRoot
	cmd.Run()

	cmd = exec.Command("git", "branch", "-D", branchName)
	cmd.Dir = o.repoRoot
	cmd.Run()
}

func (o *PageOptimizer) runClaudeCLI(worktreePath, hypothesis, change string) error {
	// Find claude binary
	claudePath := "claude"
	for _, path := range []string{
		os.Getenv("HOME") + "/.claude/local/claude",
		"/usr/local/bin/claude",
		"/opt/homebrew/bin/claude",
	} {
		if _, err := os.Stat(path); err == nil {
			claudePath = path
			break
		}
	}

	// Extract API paths from slow requests to constrain edits
	var apiPaths []string
	for _, req := range o.state.CurrentStats.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			// Extract path from URL
			if idx := strings.Index(req.URL, "://"); idx > 0 {
				rest := req.URL[idx+3:]
				if pathIdx := strings.Index(rest, "/"); pathIdx > 0 {
					path := rest[pathIdx:]
					if qIdx := strings.Index(path, "?"); qIdx > 0 {
						path = path[:qIdx]
					}
					apiPaths = append(apiPaths, path)
				}
			}
		}
	}

	// Build a specific task for Claude CLI
	task := fmt.Sprintf(`Make this optimization:

HYPOTHESIS: %s

CHANGE: %s

PAGE CONTEXT:
You are optimizing this specific page URL: %s

SCOPE: Only edit components, pages, and functions that are used to render THIS page. 
- Trace from the route to find what components render this page
- You may edit shared utilities/hooks that this page uses
- Do NOT edit other pages or components that are not part of rendering this URL

API calls made by this page:
%s

Instructions:
1. Find the route for this URL and trace which components render it
2. Make targeted changes only to code that runs when loading this page
3. Exit when done`, hypothesis, change, o.page.URL, strings.Join(apiPaths, "\n"))

	cmd := exec.Command(claudePath,
		"--print",
		"--verbose",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		task,
	)
	cmd.Dir = worktreePath

	// Parse stream-json and print cleaner output
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer for large lines
	
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}

		eventType, _ := event["type"].(string)
		switch eventType {
		case "assistant":
			// Check for tool use
			if msg, ok := event["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].([]interface{}); ok {
					for _, c := range content {
						if item, ok := c.(map[string]interface{}); ok {
							if item["type"] == "tool_use" {
								name, _ := item["name"].(string)
								if input, ok := item["input"].(map[string]interface{}); ok {
									if desc, ok := input["description"].(string); ok {
										fmt.Printf("\n  → %s: %s", name, desc)
									} else if name == "Read" || name == "Grep" || name == "Glob" {
										if path, ok := input["path"].(string); ok {
											fmt.Printf("\n  → %s: %s", name, path)
										} else if pattern, ok := input["pattern"].(string); ok {
											fmt.Printf("\n  → %s: %s", name, pattern)
										}
									} else if name == "Edit" || name == "Write" {
										if path, ok := input["file_path"].(string); ok {
											fmt.Printf("\n  → %s: %s", name, path)
										} else if path, ok := input["path"].(string); ok {
											fmt.Printf("\n  → %s: %s", name, path)
										}
									} else if name == "Bash" {
										if cmd, ok := input["command"].(string); ok {
											if len(cmd) > 60 {
												cmd = cmd[:60] + "..."
											}
											fmt.Printf("\n  → %s: %s", name, cmd)
										}
									} else {
										fmt.Printf("\n  → %s", name)
									}
								} else {
									fmt.Printf("\n  → %s", name)
								}
							}
						}
					}
				}
			}
		case "system":
			subtype, _ := event["subtype"].(string)
			if subtype == "task_started" {
				desc, _ := event["description"].(string)
				if desc != "" {
					fmt.Printf("\n  ⚡ Agent: %s", desc)
				}
			}
		}
	}
	fmt.Println() // Final newline

	return cmd.Wait()
}

func (o *PageOptimizer) commitWorktreeChanges(worktreePath string) (string, []string, error) {
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = worktreePath
	if err := cmd.Run(); err != nil {
		return "", nil, err
	}

	cmd = exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return "", nil, err
	}

	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}

	if len(files) == 0 {
		return "", nil, nil
	}

	cmd = exec.Command("git", "commit", "-m", "autoprobe: optimization attempt")
	cmd.Dir = worktreePath
	if err := cmd.Run(); err != nil {
		return "", nil, err
	}

	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktreePath
	output, err = cmd.Output()
	if err != nil {
		return "", nil, err
	}

	return strings.TrimSpace(string(output)), files, nil
}

func (o *PageOptimizer) cherryPick(commitHash string) error {
	cmd := exec.Command("git", "cherry-pick", "--no-commit", commitHash)
	cmd.Dir = o.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cherry-pick failed: %w\n%s", err, output)
	}
	return nil
}

func (o *PageOptimizer) revertCherryPick() {
	cmd := exec.Command("git", "reset", "--hard", "HEAD")
	cmd.Dir = o.repoRoot
	cmd.Run()
}

func (o *PageOptimizer) commitChanges(message string) (string, error) {
	// Stage all changes
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = o.repoRoot
	if err := cmd.Run(); err != nil {
		return "", err
	}

	// Commit
	cmd = exec.Command("git", "commit", "-m", message)
	cmd.Dir = o.repoRoot
	if err := cmd.Run(); err != nil {
		return "", err
	}

	// Get commit hash
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = o.repoRoot
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
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

func (o *PageOptimizer) compareXHRTimings(before, after *pagebench.PageStats) (bool, float64, float64, int, int) {
	beforeTotal := time.Duration(0)
	afterTotal := time.Duration(0)
	beforeXHRCount := 0
	afterXHRCount := 0

	for _, req := range before.Requests {
		if (req.ResourceType == "xhr" || req.ResourceType == "fetch") && !pagebench.IsDevToolingURL(req.URL) {
			beforeTotal += req.Duration
			beforeXHRCount++
		}
	}

	for _, req := range after.Requests {
		if (req.ResourceType == "xhr" || req.ResourceType == "fetch") && !pagebench.IsDevToolingURL(req.URL) {
			afterTotal += req.Duration
			afterXHRCount++
		}
	}

	beforeMs := float64(beforeTotal.Milliseconds())
	afterMs := float64(afterTotal.Milliseconds())

	beforeRedundant := 0
	afterRedundant := 0
	for _, dup := range before.RedundantXHR {
		if dup.Identical {
			beforeRedundant += dup.Count - 1
		}
	}
	for _, dup := range after.RedundantXHR {
		if dup.Identical {
			afterRedundant += dup.Count - 1
		}
	}

	timingImproved := (beforeMs-afterMs)/beforeMs > 0.05
	notSlowerThan10Pct := afterMs <= beforeMs*1.10
	reducedRedundant := afterRedundant < beforeRedundant && notSlowerThan10Pct
	reducedRequests := afterXHRCount < beforeXHRCount && notSlowerThan10Pct

	return timingImproved || reducedRedundant || reducedRequests, beforeMs, afterMs, beforeXHRCount, afterXHRCount
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
				fmt.Printf("  ✓ %s\n    Files: %s\n", a.Hypothesis, strings.Join(a.FilesChanged, ", "))
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

func truncateURLMiddle(url string, maxLen int) string {
	if len(url) <= maxLen {
		return url
	}
	keepStart := 25
	keepEnd := maxLen - keepStart - 3
	if keepEnd < 20 {
		keepEnd = 20
	}
	return url[:keepStart] + "..." + url[len(url)-keepEnd:]
}
