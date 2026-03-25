package analyzer

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/marccampbell/autoprobe/pkg/benchmark"
	"github.com/marccampbell/autoprobe/pkg/config"
)

// CheckClaudeCLI verifies claude cli is installed
func CheckClaudeCLI() error {
	_, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude cli not found. Install from: https://github.com/anthropics/claude-code")
	}
	return nil
}

// AnalyzeEndpoint uses Claude to analyze the code path for an endpoint
func AnalyzeEndpoint(cfg *config.Config, endpointName string, endpoint *config.EndpointConfig, baseline *benchmark.Stats, dryRun bool) error {
	prompt := buildAnalysisPrompt(cfg, endpointName, endpoint, baseline, dryRun)

	// Build claude command
	args := []string{
		"-p", prompt,
		"--allowedTools", "Read,Write,Edit,Glob,Grep,Bash",
	}

	if dryRun {
		// In dry-run mode, don't allow writes
		args = []string{
			"-p", prompt,
			"--allowedTools", "Read,Glob,Grep,Bash",
		}
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir, _ = os.Getwd()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	return cmd.Run()
}

func buildAnalysisPrompt(cfg *config.Config, endpointName string, endpoint *config.EndpointConfig, baseline *benchmark.Stats, dryRun bool) string {
	var b bytes.Buffer

	b.WriteString("You are autoprobe, an AI performance optimizer. Your task is to analyze and optimize a slow API endpoint.\n\n")

	// Endpoint info
	b.WriteString("## Target Endpoint\n\n")
	b.WriteString(fmt.Sprintf("- Name: %s\n", endpointName))
	b.WriteString(fmt.Sprintf("- Method: %s\n", endpoint.Method))
	b.WriteString(fmt.Sprintf("- URL: %s\n", endpoint.URL))
	if endpoint.Target.Duration() > 0 {
		b.WriteString(fmt.Sprintf("- Target latency: %s\n", endpoint.Target.Duration()))
	}
	b.WriteString("\n")

	// Baseline stats
	if baseline != nil {
		b.WriteString("## Current Performance (Baseline)\n\n")
		b.WriteString(fmt.Sprintf("- Requests: %d\n", baseline.Requests))
		b.WriteString(fmt.Sprintf("- p50: %.1fms\n", baseline.P50Ms))
		b.WriteString(fmt.Sprintf("- p95: %.1fms\n", baseline.P95Ms))
		b.WriteString(fmt.Sprintf("- p99: %.1fms\n", baseline.P99Ms))
		if baseline.StatusFailures > 0 {
			b.WriteString(fmt.Sprintf("- Status failures: %d\n", baseline.StatusFailures))
		}
		b.WriteString("\n")
	}

	// Database info
	if len(cfg.Databases) > 0 {
		b.WriteString("## Databases\n\n")
		b.WriteString("The following databases are configured (you can use these for query analysis):\n")
		for name, db := range cfg.Databases {
			b.WriteString(fmt.Sprintf("- %s: %s\n", name, db.Driver))
		}
		b.WriteString("\n")
	}

	// Rules
	if cfg.Rules != "" {
		b.WriteString("## Rules (You MUST follow these)\n\n")
		b.WriteString(cfg.Rules)
		b.WriteString("\n\n")
	}

	// Instructions
	b.WriteString("## Your Task\n\n")
	b.WriteString("1. **Find the code**: Locate the handler for this endpoint. Use Glob and Grep to search, then Read to examine files.\n")
	b.WriteString("2. **Trace the path**: Follow the code from handler through services, repositories, and database queries.\n")
	b.WriteString("3. **Identify bottlenecks**: Look for N+1 queries, missing indexes, inefficient algorithms, unnecessary allocations.\n")
	b.WriteString("4. **Analyze queries**: If you find SQL queries, analyze them for optimization opportunities.\n")

	if dryRun {
		b.WriteString("5. **Report findings**: This is a DRY RUN. Do NOT modify any files. Just report what you found and what you would change.\n")
	} else {
		b.WriteString("5. **Apply fixes**: Make the necessary code changes to improve performance. Be surgical — small, targeted changes.\n")
		b.WriteString("6. **Explain changes**: After each change, briefly explain what you did and why.\n")
	}

	b.WriteString("\nStart by exploring the project structure to find the route definitions.\n")

	return b.String()
}

// RunOptimizationLoop runs the full optimization cycle
func RunOptimizationLoop(cfg *config.Config, endpointName string, endpoint *config.EndpointConfig, maxIterations int, dryRun bool) error {
	iteration := 0

	for {
		iteration++
		if maxIterations > 0 && iteration > maxIterations {
			fmt.Printf("\nReached max iterations (%d)\n", maxIterations)
			break
		}

		fmt.Printf("\n=== Iteration %d ===\n", iteration)

		// Run baseline benchmark
		fmt.Println("\nRunning baseline benchmark...")
		opts := benchmark.Options{
			Requests:    benchmark.DefaultRequestsForMethod(endpoint.Method),
			Concurrency: 1,
			Delay:       50 * time.Millisecond,
		}
		baseline, err := benchmark.Run(endpointName, endpoint, opts)
		if err != nil {
			return fmt.Errorf("benchmark failed: %w", err)
		}
		benchmark.PrintStats(baseline)

		// Check if target met
		if endpoint.Target.Duration() > 0 && baseline.TargetMet {
			fmt.Printf("\n✓ Target met! p95 (%.1fms) is under target (%dms)\n", baseline.P95Ms, baseline.TargetMs)
			break
		}

		// Run analysis
		fmt.Println("\nAnalyzing endpoint...")
		if err := AnalyzeEndpoint(cfg, endpointName, endpoint, baseline, dryRun); err != nil {
			return fmt.Errorf("analysis failed: %w", err)
		}

		if dryRun {
			fmt.Println("\nDry run complete. No changes applied.")
			break
		}

		// If no target, just run once
		if endpoint.Target.Duration() == 0 {
			fmt.Println("\nNo target set. Run complete.")
			break
		}
	}

	return nil
}
