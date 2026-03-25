package cmd

import (
	"fmt"
	"time"

	"github.com/marccampbell/autoprobe/pkg/benchmark"
	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/spf13/cobra"
)

var benchmarkCmd = &cobra.Command{
	Use:   "benchmark <endpoint>",
	Short: "Run performance benchmarks on an endpoint",
	Long: `Benchmark an endpoint with configurable load.

Runs multiple requests against the specified endpoint and reports
latency statistics (min, max, mean, p50, p95, p99).

The endpoint must be defined in .autoprobe.yaml.

By default, GET/HEAD requests run 100 times, while mutating methods
(POST/PUT/PATCH/DELETE) run once. Override with --requests.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		endpointName := args[0]
		requests, _ := cmd.Flags().GetInt("requests")
		concurrency, _ := cmd.Flags().GetInt("concurrency")
		output, _ := cmd.Flags().GetString("output")

		return runBenchmark(endpointName, requests, concurrency, output)
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

func runBenchmark(endpointName string, requests, concurrency int, output string) error {
	// Load config
	cfg, err := config.LoadDefault()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Get endpoint
	endpoint, err := cfg.GetEndpoint(endpointName)
	if err != nil {
		return err
	}

	// Determine request count
	if requests == 0 {
		requests = benchmark.DefaultRequestsForMethod(endpoint.Method)
	}

	fmt.Printf("Benchmarking endpoint: %s\n", endpointName)
	fmt.Printf("  URL: %s %s\n", endpoint.Method, endpoint.URL)
	fmt.Printf("  Requests: %d\n", requests)

	// Build options
	opts := benchmark.Options{
		Requests:    requests,
		Concurrency: concurrency,
		Delay:       50 * time.Millisecond,
	}

	// Run benchmark
	stats, err := benchmark.Run(endpointName, endpoint, opts)
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
