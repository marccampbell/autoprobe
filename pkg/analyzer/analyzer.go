package analyzer

import (
	"fmt"
	"time"

	"github.com/marccampbell/autoprobe/pkg/benchmark"
	"github.com/marccampbell/autoprobe/pkg/claude"
	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/marccampbell/autoprobe/pkg/tools"
)

// AnalyzeEndpoint uses Claude to analyze the code path for an endpoint
func AnalyzeEndpoint(cfg *config.Config, endpointName string, endpoint *config.EndpointConfig, baseline *benchmark.Stats, dryRun bool) error {
	client, err := claude.NewClient()
	if err != nil {
		return err
	}

	systemPrompt := buildSystemPrompt(cfg, dryRun)
	userPrompt := buildUserPrompt(endpointName, endpoint, baseline, dryRun)
	availableTools := tools.GetTools(!dryRun) // Allow writes only if not dry-run

	return client.RunWithTools(systemPrompt, userPrompt, availableTools, nil)
}

func buildSystemPrompt(cfg *config.Config, dryRun bool) string {
	prompt := `You are autoprobe, an AI performance optimizer. Your task is to analyze and optimize slow API endpoints.

You have access to tools for reading and searching code. Use them to:
1. Find route/handler definitions
2. Trace code paths from handler to database
3. Identify performance bottlenecks
4. Suggest or apply optimizations

Be methodical. Start by understanding the project structure, then trace the specific endpoint.`

	if cfg.Rules != "" {
		prompt += "\n\n## Rules (You MUST follow these)\n\n" + cfg.Rules
	}

	if dryRun {
		prompt += "\n\nThis is a DRY RUN. Analyze and report findings, but do NOT modify any files."
	}

	return prompt
}

func buildUserPrompt(endpointName string, endpoint *config.EndpointConfig, baseline *benchmark.Stats, dryRun bool) string {
	prompt := fmt.Sprintf(`Analyze and optimize this endpoint:

## Target Endpoint
- Name: %s
- Method: %s
- URL: %s
`, endpointName, endpoint.Method, endpoint.URL)

	if endpoint.Target.Duration() > 0 {
		prompt += fmt.Sprintf("- Target latency: %s\n", endpoint.Target.Duration())
	}

	if baseline != nil {
		prompt += fmt.Sprintf(`
## Current Performance (Baseline)
- Requests: %d
- p50: %.1fms
- p95: %.1fms  
- p99: %.1fms
`, baseline.Requests, baseline.P50Ms, baseline.P95Ms, baseline.P99Ms)

		if baseline.StatusFailures > 0 {
			prompt += fmt.Sprintf("- Status failures: %d\n", baseline.StatusFailures)
		}
	}

	prompt += `
## Your Task

1. **Find the code**: Use list_files and grep to locate the route handler for this endpoint
2. **Trace the path**: Read the handler file, follow calls to services/repositories
3. **Identify bottlenecks**: Look for N+1 queries, missing indexes, inefficient loops, unnecessary allocations
4. **Analyze SQL**: If you find database queries, analyze them for optimization
`

	if dryRun {
		prompt += "\n5. **Report findings**: List what you found and what optimizations you would recommend. Do NOT modify files."
	} else {
		prompt += "\n5. **Apply fixes**: Make targeted code changes to improve performance. Explain each change."
	}

	prompt += "\n\nStart by exploring the project structure."

	return prompt
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
