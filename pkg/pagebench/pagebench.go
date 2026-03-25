package pagebench

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/playwright-community/playwright-go"
)

// RequestInfo holds info about a single network request
type RequestInfo struct {
	URL        string
	Method     string
	Status     int
	StartTime  time.Time
	Duration   time.Duration
	Size       int64
	ResourceType string
}

// PageStats holds aggregate stats for a page load
type PageStats struct {
	URL              string
	TTFB             time.Duration // Time to first byte
	DOMContentLoaded time.Duration
	FullyLoaded      time.Duration
	TotalRequests    int
	TotalSize        int64
	Requests         []RequestInfo
	Duplicates       map[string]int // URL -> count
	SlowRequests     []RequestInfo  // > 500ms
	ByType           map[string]int // xhr, fetch, script, etc.
}

// Run captures and analyzes a page load
func Run(name string, page *config.PageConfig) (*PageStats, error) {
	// Install playwright browsers if needed
	if err := playwright.Install(); err != nil {
		return nil, fmt.Errorf("failed to install playwright: %w", err)
	}

	pw, err := playwright.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to start playwright: %w", err)
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to launch browser: %w", err)
	}
	defer browser.Close()

	context, err := browser.NewContext()
	if err != nil {
		return nil, fmt.Errorf("failed to create context: %w", err)
	}

	// Set cookies if provided
	if len(page.Cookies) > 0 {
		var cookies []playwright.OptionalCookie
		for _, c := range page.Cookies {
			domain := c.Domain
			if domain == "" {
				// Extract domain from URL
				domain = extractDomain(page.URL)
			}
			cookies = append(cookies, playwright.OptionalCookie{
				Name:     c.Name,
				Value:    c.Value,
				Domain:   playwright.String(domain),
				Path:     playwright.String(c.Path),
				Secure:   playwright.Bool(c.Secure),
				HttpOnly: playwright.Bool(c.HttpOnly),
			})
		}
		if err := context.AddCookies(cookies); err != nil {
			return nil, fmt.Errorf("failed to set cookies: %w", err)
		}
	}

	pg, err := context.NewPage()
	if err != nil {
		return nil, fmt.Errorf("failed to create page: %w", err)
	}

	// Set extra headers if provided
	if len(page.Headers) > 0 {
		pg.SetExtraHTTPHeaders(page.Headers)
	}

	// Track requests
	var requests []RequestInfo
	requestStart := make(map[string]time.Time)

	pg.OnRequest(func(req playwright.Request) {
		requestStart[req.URL()] = time.Now()
	})

	pg.OnResponse(func(resp playwright.Response) {
		url := resp.URL()
		start, ok := requestStart[url]
		if !ok {
			start = time.Now()
		}
		
		req := resp.Request()
		
		// Get size from headers (don't call Body() in callback - causes deadlock)
		headers, _ := resp.AllHeaders()
		var size int64
		if cl, ok := headers["content-length"]; ok {
			fmt.Sscanf(cl, "%d", &size)
		}

		requests = append(requests, RequestInfo{
			URL:          url,
			Method:       req.Method(),
			Status:       resp.Status(),
			StartTime:    start,
			Duration:     time.Since(start),
			Size:         size,
			ResourceType: req.ResourceType(),
		})
	})

	// Navigate
	pageStart := time.Now()
	_, err = pg.Goto(page.URL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to navigate: %w", err)
	}
	fullyLoaded := time.Since(pageStart)

	// Set localStorage if provided (after page load)
	if len(page.LocalStorage) > 0 {
		for key, value := range page.LocalStorage {
			pg.Evaluate(fmt.Sprintf(`localStorage.setItem(%q, %q)`, key, value))
		}
	}

	// Calculate stats
	stats := &PageStats{
		URL:           page.URL,
		FullyLoaded:   fullyLoaded,
		TotalRequests: len(requests),
		Requests:      requests,
		Duplicates:    make(map[string]int),
		ByType:        make(map[string]int),
	}

	// Find TTFB (first response time)
	if len(requests) > 0 {
		// Sort by start time
		sorted := make([]RequestInfo, len(requests))
		copy(sorted, requests)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].StartTime.Before(sorted[j].StartTime)
		})
		stats.TTFB = sorted[0].Duration
	}

	// Analyze requests
	urlCounts := make(map[string]int)
	for _, req := range requests {
		stats.TotalSize += req.Size
		
		// Count by URL (for duplicates)
		urlCounts[req.URL]++
		
		// Count by type
		stats.ByType[req.ResourceType]++
		
		// Track slow requests (> 500ms)
		if req.Duration > 500*time.Millisecond {
			stats.SlowRequests = append(stats.SlowRequests, req)
		}
	}

	// Find duplicates (URL called more than once)
	for url, count := range urlCounts {
		if count > 1 {
			stats.Duplicates[url] = count
		}
	}

	return stats, nil
}

// PrintStats outputs page stats to terminal
func PrintStats(stats *PageStats) {
	fmt.Printf("\nPage: %s\n", stats.URL)
	fmt.Println(strings.Repeat("-", 60))
	
	fmt.Printf("\nTiming:\n")
	fmt.Printf("  Time to First Byte: %s\n", stats.TTFB.Round(time.Millisecond))
	fmt.Printf("  Fully Loaded:       %s\n", stats.FullyLoaded.Round(time.Millisecond))
	
	fmt.Printf("\nRequests: %d total (%.1f KB)\n", stats.TotalRequests, float64(stats.TotalSize)/1024)
	
	// By type
	if len(stats.ByType) > 0 {
		fmt.Printf("\nBy Type:\n")
		for typ, count := range stats.ByType {
			fmt.Printf("  %-12s %d\n", typ, count)
		}
	}
	
	// Duplicates
	if len(stats.Duplicates) > 0 {
		fmt.Printf("\n⚠ Duplicate Requests (%d):\n", len(stats.Duplicates))
		for url, count := range stats.Duplicates {
			// Truncate long URLs
			displayURL := url
			if len(displayURL) > 60 {
				displayURL = displayURL[:57] + "..."
			}
			fmt.Printf("  %dx %s\n", count, displayURL)
		}
	}
	
	// Slow requests
	if len(stats.SlowRequests) > 0 {
		fmt.Printf("\n⚠ Slow Requests (>500ms):\n")
		// Sort by duration
		sort.Slice(stats.SlowRequests, func(i, j int) bool {
			return stats.SlowRequests[i].Duration > stats.SlowRequests[j].Duration
		})
		for _, req := range stats.SlowRequests {
			displayURL := req.URL
			if len(displayURL) > 50 {
				displayURL = displayURL[:47] + "..."
			}
			fmt.Printf("  %s %s (%s)\n", req.Method, displayURL, req.Duration.Round(time.Millisecond))
		}
	}
}

func extractDomain(url string) string {
	// Simple extraction - find host between :// and next /
	start := strings.Index(url, "://")
	if start == -1 {
		return ""
	}
	rest := url[start+3:]
	end := strings.Index(rest, "/")
	if end == -1 {
		return rest
	}
	host := rest[:end]
	// Remove port if present
	if idx := strings.Index(host, ":"); idx != -1 {
		host = host[:idx]
	}
	return host
}
