package benchmark

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/marccampbell/autoprobe/pkg/config"
)

// Result holds the outcome of a single request
type Result struct {
	StatusCode    int
	Latency       time.Duration
	TTFB          time.Duration
	ResponseSize  int64
	Error         error
	StatusSuccess bool // Whether status code matched expectation
}

// Stats holds aggregate statistics from a benchmark run
type Stats struct {
	Endpoint       string
	URL            string
	Method         string
	Requests       int
	Errors         int    // Connection/request errors
	StatusFailures int    // Unexpected status codes
	ExpectedStatus int    // What status we expected
	Duration       time.Duration
	TargetMs       int64
	TargetMet      bool
	MinMs          float64
	MaxMs          float64
	MeanMs         float64
	StdDevMs       float64
	P50Ms          float64
	P95Ms          float64
	P99Ms          float64
	RequestsPerSec float64
}

// Options configures a benchmark run
type Options struct {
	Requests    int
	Concurrency int
	Delay       time.Duration
}

// DefaultOptions returns sensible defaults
func DefaultOptions() Options {
	return Options{
		Requests:    100,
		Concurrency: 1,
		Delay:       50 * time.Millisecond,
	}
}

// DefaultRequestsForMethod returns the default request count based on HTTP method
func DefaultRequestsForMethod(method string) int {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "PATCH", "DELETE":
		return 1
	default:
		return 100
	}
}

// Run executes a benchmark against the given endpoint
func Run(name string, endpoint *config.EndpointConfig, opts Options) (*Stats, error) {
	results := make([]Result, 0, opts.Requests)
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Determine expected status code
	expectedStatus := endpoint.Expect
	if expectedStatus == 0 {
		expectedStatus = 200 // Default to 200, but we'll accept any 2xx
	}

	startTime := time.Now()

	for i := 0; i < opts.Requests; i++ {
		result := makeRequest(client, endpoint, expectedStatus)
		results = append(results, result)

		// Delay between requests (unless last one)
		if i < opts.Requests-1 && opts.Delay > 0 {
			time.Sleep(opts.Delay)
		}
	}

	totalDuration := time.Since(startTime)

	return calculateStats(name, endpoint, results, totalDuration, expectedStatus), nil
}

func makeRequest(client *http.Client, endpoint *config.EndpointConfig, expectedStatus int) Result {
	method := endpoint.Method
	if method == "" {
		method = "GET"
	}

	var body io.Reader
	if endpoint.Body != "" {
		body = strings.NewReader(endpoint.Body)
	}

	req, err := http.NewRequest(method, endpoint.URL, body)
	if err != nil {
		return Result{Error: err}
	}

	// Add headers
	for k, v := range endpoint.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Error: err, Latency: time.Since(start)}
	}
	ttfb := time.Since(start)

	// Read full response body
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	latency := time.Since(start)

	// Check status code
	statusSuccess := false
	if endpoint.Expect != 0 {
		// Exact match required
		statusSuccess = resp.StatusCode == expectedStatus
	} else {
		// Accept any 2xx
		statusSuccess = resp.StatusCode >= 200 && resp.StatusCode < 300
	}

	return Result{
		StatusCode:    resp.StatusCode,
		Latency:       latency,
		TTFB:          ttfb,
		ResponseSize:  int64(len(bodyBytes)),
		Error:         nil,
		StatusSuccess: statusSuccess,
	}
}

func calculateStats(name string, endpoint *config.EndpointConfig, results []Result, totalDuration time.Duration, expectedStatus int) *Stats {
	stats := &Stats{
		Endpoint:       name,
		URL:            endpoint.URL,
		Method:         endpoint.Method,
		Requests:       len(results),
		Duration:       totalDuration,
		TargetMs:       int64(endpoint.Target.Duration() / time.Millisecond),
		ExpectedStatus: expectedStatus,
	}

	if stats.Method == "" {
		stats.Method = "GET"
	}

	// Collect latencies, count errors and status failures
	latencies := make([]float64, 0, len(results))
	for _, r := range results {
		if r.Error != nil {
			stats.Errors++
			continue
		}
		if !r.StatusSuccess {
			stats.StatusFailures++
		}
		latencies = append(latencies, float64(r.Latency.Microseconds()) / 1000.0)
	}

	if len(latencies) == 0 {
		return stats
	}

	// Sort for percentiles
	sort.Float64s(latencies)

	// Min/Max
	stats.MinMs = latencies[0]
	stats.MaxMs = latencies[len(latencies)-1]

	// Mean
	var sum float64
	for _, l := range latencies {
		sum += l
	}
	stats.MeanMs = sum / float64(len(latencies))

	// Std dev
	var variance float64
	for _, l := range latencies {
		diff := l - stats.MeanMs
		variance += diff * diff
	}
	if len(latencies) > 1 {
		variance /= float64(len(latencies) - 1)
		stats.StdDevMs = sqrt(variance)
	}

	// Percentiles
	stats.P50Ms = percentile(latencies, 50)
	stats.P95Ms = percentile(latencies, 95)
	stats.P99Ms = percentile(latencies, 99)

	// Requests per second
	if totalDuration > 0 {
		stats.RequestsPerSec = float64(len(results)) / totalDuration.Seconds()
	}

	// Target check (against p95)
	if stats.TargetMs > 0 {
		stats.TargetMet = stats.P95Ms <= float64(stats.TargetMs)
	}

	return stats
}

func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	
	rank := float64(p) / 100.0 * float64(len(sorted)-1)
	lower := int(rank)
	upper := lower + 1
	if upper >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	
	weight := rank - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x / 2
	for i := 0; i < 10; i++ {
		z = z - (z*z-x)/(2*z)
	}
	return z
}
