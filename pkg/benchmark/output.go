package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
)

// PrintStats outputs stats to terminal
func PrintStats(stats *Stats) {
	fmt.Printf("\nEndpoint: %s (%s %s)\n", stats.Endpoint, stats.Method, stats.URL)
	
	// Build status line
	statusLine := fmt.Sprintf("Requests: %d", stats.Requests)
	if stats.Errors > 0 {
		statusLine += fmt.Sprintf(" | Errors: %d", stats.Errors)
	}
	if stats.StatusFailures > 0 {
		statusLine += fmt.Sprintf(" | Status failures: %d", stats.StatusFailures)
	}
	statusLine += fmt.Sprintf(" | Duration: %.1fs", stats.Duration.Seconds())
	fmt.Println(statusLine)
	
	if stats.Requests == stats.Errors {
		fmt.Println("\nAll requests failed!")
		return
	}
	
	if stats.StatusFailures > 0 {
		if stats.ExpectedStatus != 0 {
			fmt.Printf("\n⚠ Expected status %d but got failures\n", stats.ExpectedStatus)
		} else {
			fmt.Println("\n⚠ Some requests returned non-2xx status codes")
		}
	}

	fmt.Println("\nLatency:")
	fmt.Printf("  min     %.1fms\n", stats.MinMs)
	fmt.Printf("  max     %.1fms\n", stats.MaxMs)
	fmt.Printf("  mean    %.1fms\n", stats.MeanMs)
	fmt.Printf("  p50     %.1fms\n", stats.P50Ms)
	fmt.Printf("  p95     %.1fms\n", stats.P95Ms)
	fmt.Printf("  p99     %.1fms\n", stats.P99Ms)

	if stats.TargetMs > 0 {
		fmt.Println()
		if stats.TargetMet {
			fmt.Printf("Target: %dms ✓ (p95 under target)\n", stats.TargetMs)
		} else {
			fmt.Printf("Target: %dms ✗ (p95 %.1fms exceeds target)\n", stats.TargetMs, stats.P95Ms)
		}
	}
}

// JSONOutput is the structure for JSON output
type JSONOutput struct {
	Endpoint       string  `json:"endpoint"`
	URL            string  `json:"url"`
	Method         string  `json:"method"`
	Requests       int     `json:"requests"`
	Errors         int     `json:"errors"`
	StatusFailures int     `json:"status_failures"`
	ExpectedStatus int     `json:"expected_status"`
	DurationS      float64 `json:"duration_s"`
	Latency        struct {
		MinMs    float64 `json:"min_ms"`
		MaxMs    float64 `json:"max_ms"`
		MeanMs   float64 `json:"mean_ms"`
		StdDevMs float64 `json:"stddev_ms"`
		P50Ms    float64 `json:"p50_ms"`
		P95Ms    float64 `json:"p95_ms"`
		P99Ms    float64 `json:"p99_ms"`
	} `json:"latency"`
	TargetMs       int64   `json:"target_ms,omitempty"`
	TargetMet      bool    `json:"target_met,omitempty"`
	RequestsPerSec float64 `json:"requests_per_sec"`
}

// WriteJSON writes stats to a JSON file
func WriteJSON(stats *Stats, path string) error {
	out := JSONOutput{
		Endpoint:       stats.Endpoint,
		URL:            stats.URL,
		Method:         stats.Method,
		Requests:       stats.Requests,
		Errors:         stats.Errors,
		StatusFailures: stats.StatusFailures,
		ExpectedStatus: stats.ExpectedStatus,
		DurationS:      stats.Duration.Seconds(),
		TargetMs:       stats.TargetMs,
		TargetMet:      stats.TargetMet,
		RequestsPerSec: stats.RequestsPerSec,
	}
	out.Latency.MinMs = stats.MinMs
	out.Latency.MaxMs = stats.MaxMs
	out.Latency.MeanMs = stats.MeanMs
	out.Latency.StdDevMs = stats.StdDevMs
	out.Latency.P50Ms = stats.P50Ms
	out.Latency.P95Ms = stats.P95Ms
	out.Latency.P99Ms = stats.P99Ms

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal JSON: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}
