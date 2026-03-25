package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <endpoint>",
	Short: "Analyze and optimize an endpoint",
	Long: `Run the optimization loop on an endpoint.

Autoprobe will:
  1. Benchmark the endpoint to establish baseline performance
  2. Analyze the code and database queries
  3. Apply optimizations (query rewrites, index suggestions, code changes)
  4. Re-benchmark to verify improvements
  5. Repeat until target is met or max iterations reached

Changes are written to the working tree but not committed.
Review with 'git diff' and commit what you want to keep.

The endpoint must be defined in .autoprobe.yaml.

Requires ANTHROPIC_API_KEY to be set.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		endpointName := args[0]
		maxIterations, _ := cmd.Flags().GetInt("max-iterations")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		return runOptimize(endpointName, maxIterations, dryRun)
	},
}

var (
	runMaxIterations int
	runDryRun        bool
)

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().IntVar(&runMaxIterations, "max-iterations", 0, "Maximum optimization iterations (0 = until target met)")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Show proposed changes without applying them")
}

func runOptimize(endpointName string, maxIterations int, dryRun bool) error {
	// Check for API key
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return fmt.Errorf("ANTHROPIC_API_KEY not set. Get one at https://console.anthropic.com")
	}

	// TODO: Load config and validate endpoint exists
	// TODO: Load database connections
	// TODO: Run optimization loop

	fmt.Printf("Optimizing endpoint: %s\n", endpointName)
	if maxIterations > 0 {
		fmt.Printf("  Max iterations: %d\n", maxIterations)
	} else {
		fmt.Println("  Max iterations: unlimited (until target met)")
	}
	if dryRun {
		fmt.Println("  Mode: dry-run (no changes will be applied)")
	}

	fmt.Println("\n[optimization not yet implemented]")
	return nil
}
