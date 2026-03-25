package cmd

import (
	"fmt"
	"time"

	"github.com/marccampbell/autoprobe/pkg/benchmark"
	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/marccampbell/autoprobe/pkg/pagebench"
	"github.com/spf13/cobra"
)

var benchmarkCmd = &cobra.Command{
	Use:   "benchmark <name>",
	Short: "Run performance benchmarks on an endpoint or page",
	Long: `Benchmark an endpoint or page.

For endpoints: runs multiple HTTP requests and reports latency statistics.
For pages: loads in a browser and reports TTFB, load time, request count, duplicates.

The target must be defined in .autoprobe.yaml under 'endpoints' or 'pages'.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		requests, _ := cmd.Flags().GetInt("requests")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		output, _ := cmd.Flags().GetString("output")

		return runBenchmark(name, requests, concurrency, output)
	},
}

var (
	benchRequests    int
	benchConcurrency int
	benchOutput      string
)

func init() {
	rootCmd.AddCommand(benchmarkCmd)

	benchmarkCmd.Flags().IntVarP(&benchRequests, "requests", "n", 0, "Number of requests to run (default: 100 for GET, 1 for POST/PUT/PATCH/DELETE)")
	benchmarkCmd.Flags().IntVarP(&benchConcurrency, "concurrency", "c", 1, "Number of concurrent requests")
	benchmarkCmd.Flags().StringVarP(&benchOutput, "output", "o", "", "Output file for results (JSON)")
}

func runBenchmark(name string, requests, concurrency int, output string) error {
	// Load config
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Check if it's a page or endpoint
	if cfg.IsPage(name) {
		return runPageBenchmark(cfg, name)
	}

	return runEndpointBenchmark(cfg, name, requests, concurrency, output)
}

func runPageBenchmark(cfg *config.Config, name string) error {
	page, err := cfg.GetPage(name)
	if err != nil {
		return err
	}

	fmt.Printf("Benchmarking page: %s\n", name)
	fmt.Printf("  URL: %s\n", page.URL)
	fmt.Println("  Loading browser...")

	stats, err := pagebench.Run(name, page)
	if err != nil {
		return fmt.Errorf("page benchmark failed: %w", err)
	}

	pagebench.PrintStats(stats)
	return nil
}

func runEndpointBenchmark(cfg *config.Config, name string, requests, concurrency int, output string) error {
	endpoint, err := cfg.GetEndpoint(name)
	if err != nil {
		return err
	}

	// Determine request count
	if requests == 0 {
		requests = benchmark.DefaultRequestsForMethod(endpoint.Method)
	}

	fmt.Printf("Benchmarking endpoint: %s\n", name)
	fmt.Printf("  URL: %s %s\n", endpoint.Method, endpoint.URL)
	fmt.Printf("  Requests: %d\n", requests)

	// Build options
	opts := benchmark.Options{
		Requests:    requests,
		Concurrency: concurrency,
		Delay:       50 * time.Millisecond,
	}

	// Run benchmark
	stats, err := benchmark.Run(name, endpoint, opts)
	if err != nil {
		return fmt.Errorf("benchmark failed: %w", err)
	}

	// Output results
	benchmark.PrintStats(stats)

	// Write JSON if requested
	if output != "" {
		if err := benchmark.WriteJSON(stats, output); err != nil {
			return err
		}
		fmt.Printf("\nResults written to %s\n", output)
	}

	return nil
}
