package cmd

import (
	"fmt"

	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/marccampbell/autoprobe/pkg/optimizer"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Analyze and optimize an endpoint or page",
	Long: `Run the optimization loop on an endpoint or page.

For endpoints, Autoprobe will:
  1. Benchmark the endpoint to establish baseline performance
  2. Analyze the code and database queries
  3. Apply optimizations (query rewrites, index suggestions, code changes)
  4. Re-benchmark to verify improvements
  5. Repeat until target is met or max iterations reached

For pages, Autoprobe will:
  1. Load the page in a browser and identify slow XHR requests
  2. Analyze both client-side (React, etc.) and server-side code
  3. Apply optimizations to either client or server
  4. Re-benchmark the page to verify improvements

Changes are written to the working tree but not committed.
Review with 'git diff' and commit what you want to keep.

The target must be defined in .autoprobe.yaml under 'endpoints' or 'pages'.

Requires ANTHROPIC_API_KEY to be set.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		maxIterations, _ := cmd.Flags().GetInt("max-iterations")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		verbose, _ := cmd.Flags().GetBool("verbose")

		return runOptimize(name, maxIterations, dryRun, verbose)
	},
}

var (
	runMaxIterations int
	runDryRun        bool
	runVerbose       bool
)

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().IntVar(&runMaxIterations, "max-iterations", 0, "Maximum optimization iterations (0 = until target met)")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Show proposed changes without applying them")
	runCmd.Flags().BoolVar(&runVerbose, "verbose", false, "Show full LLM output for debugging")
}

func runOptimize(name string, maxIterations int, dryRun, verbose bool) error {
	// Load config
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if it's a page or endpoint
	if cfg.IsPage(name) {
		return runPageOptimize(cfg, name, maxIterations, dryRun, verbose)
	}

	return runEndpointOptimize(cfg, name, maxIterations, dryRun, verbose)
}

func runPageOptimize(cfg *config.Config, name string, maxIterations int, dryRun, verbose bool) error {
	page, err := cfg.GetPage(name)
	if err != nil {
		return err
	}

	fmt.Printf("Optimizing page: %s\n", name)
	fmt.Printf("  URL: %s\n", page.URL)
	if maxIterations > 0 {
		fmt.Printf("  Max iterations: %d\n", maxIterations)
	}
	if dryRun {
		fmt.Println("  Mode: dry-run (no changes will be applied)")
	}
	if cfg.Rules != "" {
		fmt.Println("  Rules: configured")
	}

	opt, err := optimizer.NewPageOptimizer(cfg, name, page, dryRun, verbose)
	if err != nil {
		return err
	}

	return opt.Run(maxIterations)
}

func runEndpointOptimize(cfg *config.Config, name string, maxIterations int, dryRun, verbose bool) error {
	endpoint, err := cfg.GetEndpoint(name)
	if err != nil {
		return err
	}

	fmt.Printf("Optimizing endpoint: %s\n", name)
	fmt.Printf("  URL: %s %s\n", endpoint.Method, endpoint.URL)
	if maxIterations > 0 {
		fmt.Printf("  Max iterations: %d\n", maxIterations)
	}
	if dryRun {
		fmt.Println("  Mode: dry-run (no changes will be applied)")
	}
	if cfg.Rules != "" {
		fmt.Println("  Rules: configured")
	}

	opt, err := optimizer.New(cfg, name, endpoint, dryRun, verbose)
	if err != nil {
		return err
	}

	return opt.Run(maxIterations)
}
