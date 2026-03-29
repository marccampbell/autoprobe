package optimizer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/marccampbell/autoprobe/pkg/pagebench"
)

// PageOptimizer runs the optimization loop for pages
type PageOptimizer struct {
	cfg     *config.Config
	page    *config.PageConfig
	name    string
	state   *PageRunState
	dryRun  bool
	verbose bool
	repoRoot string
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
	Hypothesis    string
	FilesChanged  []string
	Kept          bool
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

	return &PageOptimizer{
		cfg:      cfg,
		page:     page,
		name:     pageName,
		dryRun:   dryRun,
		verbose:  verbose,
		repoRoot: repoRoot,
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

	// Build the prompt for Claude
	prompt := o.buildPrompt(slowRequests)

	// Main optimization loop
	for {
		if maxIterations > 0 && o.state.Iteration >= maxIterations {
			fmt.Printf("\n=== Completed %d iterations ===\n", maxIterations)
			break
		}

		o.state.Iteration++
		fmt.Printf("\n=== Iteration %d ===\n", o.state.Iteration)

		if o.dryRun {
			fmt.Println("[DRY RUN] Would create worktree and run Claude")
			fmt.Printf("Prompt:\n%s\n", prompt)
			break
		}

		// Create worktree for this experiment
		worktreePath, branchName, err := o.createWorktree()
		if err != nil {
			fmt.Printf("Failed to create worktree: %v\n", err)
			break
		}

		fmt.Printf("Worktree: %s\n", worktreePath)

		// Run Claude CLI in the worktree
		fmt.Println("Running Claude...")
		err = o.runClaude(worktreePath, prompt)
		if err != nil {
			fmt.Printf("Claude failed: %v\n", err)
			o.cleanupWorktree(worktreePath, branchName)
			continue
		}

		// Commit changes in worktree
		fmt.Print("Committing changes... ")
		commitHash, changedFiles, err := o.commitWorktreeChanges(worktreePath)
		if err != nil || len(changedFiles) == 0 {
			fmt.Println("no changes made")
			o.cleanupWorktree(worktreePath, branchName)
			fmt.Println("No more optimizations identified.")
			break
		}
		fmt.Printf("done (%d files)\n", len(changedFiles))
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

		// Benchmark (now using the real dev server with cherry-picked changes)
		fmt.Print("Benchmarking (3 runs)... ")
		afterStats, err := pagebench.RunMultiple(o.name, o.page, 3, false)
		if err != nil {
			fmt.Printf("failed: %v\n", err)
			o.revertCherryPick()
			o.cleanupWorktree(worktreePath, branchName)
			continue
		}

		improved, beforeMs, afterMs, beforeCount, afterCount := o.compareXHRTimings(o.state.CurrentStats, afterStats)

		if improved {
			fmt.Printf("KEEP ✓ (%.0fms → %.0fms, %d → %d reqs)\n", beforeMs, afterMs, beforeCount, afterCount)
			// Changes are already in main, just leave them
			o.state.CurrentStats = afterStats
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				FilesChanged: changedFiles,
				Kept:         true,
			})
			prompt = o.buildPromptWithHistory(slowRequests)
		} else {
			diff := afterMs - beforeMs
			sign := "+"
			if diff < 0 {
				sign = ""
			}
			fmt.Printf("DISCARD ✗ (%.0fms → %.0fms, %s%.0fms, %d → %d reqs)\n", beforeMs, afterMs, sign, diff, beforeCount, afterCount)
			
			// Revert the cherry-pick
			o.revertCherryPick()
			
			o.state.Attempts = append(o.state.Attempts, PageAttempt{
				FilesChanged: changedFiles,
				Kept:         false,
			})
			prompt = o.buildPromptWithHistory(slowRequests)
		}

		// Cleanup worktree
		o.cleanupWorktree(worktreePath, branchName)
	}

	o.printSummary()
	return nil
}

func (o *PageOptimizer) createWorktree() (string, string, error) {
	// Create unique branch and worktree path
	branchName := fmt.Sprintf("autoprobe-exp-%d", time.Now().UnixNano())
	worktreePath := filepath.Join(os.TempDir(), branchName)

	// Create worktree from current HEAD
	cmd := exec.Command("git", "worktree", "add", "-b", branchName, worktreePath, "HEAD")
	cmd.Dir = o.repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("git worktree add failed: %w\n%s", err, output)
	}

	return worktreePath, branchName, nil
}

func (o *PageOptimizer) cleanupWorktree(worktreePath, branchName string) {
	// Remove worktree
	cmd := exec.Command("git", "worktree", "remove", "--force", worktreePath)
	cmd.Dir = o.repoRoot
	cmd.Run()

	// Delete branch
	cmd = exec.Command("git", "branch", "-D", branchName)
	cmd.Dir = o.repoRoot
	cmd.Run()
}

func (o *PageOptimizer) runClaude(worktreePath, prompt string) error {
	// Write prompt to temp file
	promptFile := filepath.Join(worktreePath, ".autoprobe-prompt.txt")
	if err := os.WriteFile(promptFile, []byte(prompt), 0644); err != nil {
		return err
	}
	defer os.Remove(promptFile)

	// Run Claude CLI
	// Look for claude in common locations
	claudePath := "claude"
	for _, path := range []string{
		"/usr/local/bin/claude",
		"/opt/homebrew/bin/claude",
		os.Getenv("HOME") + "/.npm-global/bin/claude",
		os.Getenv("HOME") + "/.local/bin/claude",
	} {
		if _, err := os.Stat(path); err == nil {
			claudePath = path
			break
		}
	}
	
	cmd := exec.Command(claudePath,
		"--print",  // Non-interactive
		"--dangerously-skip-permissions", // Allow file writes
		prompt,
	)
	cmd.Dir = worktreePath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if o.verbose {
		fmt.Println("--- Claude Output ---")
	}

	err := cmd.Run()

	if o.verbose {
		fmt.Println("--- End Claude Output ---")
	}

	return err
}

func (o *PageOptimizer) commitWorktreeChanges(worktreePath string) (string, []string, error) {
	// Stage all changes
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = worktreePath
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("git add failed: %w", err)
	}

	// Get list of staged files
	cmd = exec.Command("git", "diff", "--cached", "--name-only")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		return "", nil, err
	}

	var files []string
	for _, f := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if f != "" && !strings.HasPrefix(f, ".autoprobe") {
			files = append(files, f)
		}
	}

	if len(files) == 0 {
		return "", nil, nil
	}

	// Commit
	cmd = exec.Command("git", "commit", "-m", "autoprobe: optimization attempt")
	cmd.Dir = worktreePath
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("git commit failed: %w", err)
	}

	// Get commit hash
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
	// Reset staged changes and restore working tree
	cmd := exec.Command("git", "reset", "--hard", "HEAD")
	cmd.Dir = o.repoRoot
	cmd.Run()
}

func (o *PageOptimizer) buildPrompt(slowRequests []pagebench.RequestInfo) string {
	var sb strings.Builder

	sb.WriteString("You are optimizing a web page's frontend performance.\n\n")
	sb.WriteString("## Goal\n")
	sb.WriteString("Reduce slow or redundant XHR/fetch requests by modifying the client-side code.\n\n")

	sb.WriteString("## Slow Requests to Optimize\n")
	for _, req := range slowRequests {
		sb.WriteString(fmt.Sprintf("- %s %s (%s)\n", req.Method, req.URL, req.Duration.Round(time.Millisecond)))
	}
	sb.WriteString("\n")

	sb.WriteString("## Common Issues\n")
	sb.WriteString("1. Redundant API calls - same endpoint called multiple times\n")
	sb.WriteString("2. useEffect with missing/wrong deps causing re-fetches\n")
	sb.WriteString("3. Missing React Query/SWR caching\n")
	sb.WriteString("4. Sequential requests that could be parallel\n")
	sb.WriteString("5. Components re-rendering and triggering unnecessary fetches\n\n")

	sb.WriteString("## Instructions\n")
	sb.WriteString("1. Investigate the codebase to find the components making these requests\n")
	sb.WriteString("2. Identify ONE optimization opportunity\n")
	sb.WriteString("3. Make the necessary code changes (may be multiple files)\n")
	sb.WriteString("4. Focus on client-side .tsx/.jsx/.ts/.js files\n")
	sb.WriteString("5. Do NOT modify server-side/API code\n\n")

	sb.WriteString("Make your changes, then exit when done.\n")

	return sb.String()
}

func (o *PageOptimizer) buildPromptWithHistory(slowRequests []pagebench.RequestInfo) string {
	var sb strings.Builder

	sb.WriteString(o.buildPrompt(slowRequests))

	if len(o.state.Attempts) > 0 {
		sb.WriteString("\n## Previous Attempts (DO NOT REPEAT)\n")
		for i, a := range o.state.Attempts {
			status := "DISCARDED"
			if a.Kept {
				status = "KEPT"
			}
			sb.WriteString(fmt.Sprintf("%d. [%s] Changed: %s\n", i+1, status, strings.Join(a.FilesChanged, ", ")))
		}
		sb.WriteString("\nFind a DIFFERENT optimization.\n")
	}

	return sb.String()
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

	// Count redundant (identical) XHR requests
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
				fmt.Printf("  ✓ %s\n", strings.Join(a.FilesChanged, ", "))
			}
		}
	}
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
