package pagebench

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/playwright-community/playwright-go"
)

// RequestInfo holds info about a single network request
type RequestInfo struct {
	URL          string
	Method       string
	Status       int
	StartTime    time.Time
	Duration     time.Duration
	Size         int64
	ResourceType string
	BodyHash     string // SHA256 of response body (for XHR/fetch only)
	HasEtag      bool   // True if response has ETag header
	HasVary      bool   // True if response has Vary header
}

// DuplicateInfo describes a set of duplicate requests
type DuplicateInfo struct {
	URL          string
	Count        int
	Identical    bool   // True if all responses have same body hash
	TotalTimeMs  int64  // Combined time spent on these requests
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
	Duplicates       map[string]int        // URL -> count (legacy)
	RedundantXHR     []DuplicateInfo       // XHR requests with identical responses
	SlowRequests     []RequestInfo         // > 500ms
	ByType           map[string]int        // xhr, fetch, script, etc.
}

// Run captures and analyzes a page load
func Run(name string, page *config.PageConfig, verbose bool) (*PageStats, error) {
	// Install playwright browsers if needed (suppress noisy output)
	installOpts := &playwright.RunOptions{
		Browsers: []string{"chromium"},
		Verbose:  false,
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	}
	if err := playwright.Install(installOpts); err != nil {
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

	// Track requests (need mutex since callbacks run concurrently)
	var requests []RequestInfo
	var mu sync.Mutex
	// Use request pointer address as unique key to handle duplicate URLs
	requestStart := make(map[playwright.Request]time.Time)
	// Store response bodies for XHR (to detect duplicates)
	xhrBodies := make(map[string][]string) // URL -> list of body hashes
	
	// Capture console messages (only in verbose mode)
	if verbose {
		pg.OnConsole(func(msg playwright.ConsoleMessage) {
			msgType := msg.Type()
			if msgType == "error" || msgType == "warning" {
				fmt.Printf("  [CONSOLE %s] %s\n", strings.ToUpper(msgType), msg.Text())
			}
		})
		
		pg.OnPageError(func(err error) {
			fmt.Printf("  [PAGE ERROR] %s\n", err.Error())
		})
	}

	// Listen at context level to catch all requests including from workers/subframes
	context.OnRequest(func(req playwright.Request) {
		if verbose {
			fmt.Printf("  [DEBUG] Request: %s %s\n", req.Method(), req.URL())
		}
		mu.Lock()
		requestStart[req] = time.Now()
		mu.Unlock()
	})

	context.OnResponse(func(resp playwright.Response) {
		url := resp.URL()
		req := resp.Request()
		resType := req.ResourceType()
		
		if verbose {
			fmt.Printf("  [DEBUG] Response: %d %s (%s)\n", resp.Status(), url, resType)
		}
		
		mu.Lock()
		start, ok := requestStart[req]
		if !ok {
			start = time.Now()
		}
		delete(requestStart, req) // Clean up
		mu.Unlock()

		info := RequestInfo{
			URL:          url,
			Method:       req.Method(),
			Status:       resp.Status(),
			StartTime:    start,
			Duration:     time.Since(start),
			Size:         0,
			ResourceType: resType,
		}
		
		// For XHR/fetch, try to get body hash and check headers
		if resType == "xhr" || resType == "fetch" {
			// Check for cache-related headers (can't call in callback, do async)
			go func() {
				headers, err := resp.AllHeaders()
				if err == nil {
					if _, ok := headers["etag"]; ok {
						mu.Lock()
						info.HasEtag = true
						mu.Unlock()
					}
					if _, ok := headers["vary"]; ok {
						mu.Lock()
						info.HasVary = true
						mu.Unlock()
					}
				}
				
				// Get body hash
				body, err := resp.Body()
				if err == nil && len(body) > 0 {
					hash := fmt.Sprintf("%x", sha256.Sum256(body))[:16] // First 16 chars of SHA256
					mu.Lock()
					info.BodyHash = hash
					xhrBodies[url] = append(xhrBodies[url], hash)
					mu.Unlock()
				}
			}()
		}
		
		mu.Lock()
		requests = append(requests, info)
		mu.Unlock()
	})

	// If localStorage/sessionStorage needed, navigate to origin first to set it
	if len(page.LocalStorage) > 0 || len(page.SessionStorage) > 0 {
		// Navigate to origin to establish context
		origin := extractOrigin(page.URL)
		pg.Goto(origin, playwright.PageGotoOptions{
			WaitUntil: playwright.WaitUntilStateCommit,
		})
		
		// Set localStorage
		for key, value := range page.LocalStorage {
			pg.Evaluate(fmt.Sprintf(`localStorage.setItem(%q, %q)`, key, value))
		}
		
		// Set sessionStorage
		for key, value := range page.SessionStorage {
			pg.Evaluate(fmt.Sprintf(`sessionStorage.setItem(%q, %q)`, key, value))
		}
	}

	// Navigate to actual page
	pageStart := time.Now()
	_, err = pg.Goto(page.URL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateDomcontentloaded,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to navigate: %w", err)
	}
	
	// Wait for network to settle
	pg.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
		State: playwright.LoadStateNetworkidle,
	})
	
	fullyLoaded := time.Since(pageStart)
	




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

	// Analyze XHR duplicates for identical responses
	// Wait a moment for async body hash collection to complete
	time.Sleep(100 * time.Millisecond)
	
	mu.Lock()
	xhrByURL := make(map[string][]RequestInfo)
	for _, req := range requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			xhrByURL[req.URL] = append(xhrByURL[req.URL], req)
		}
	}
	mu.Unlock()
	
	for url, reqs := range xhrByURL {
		if len(reqs) > 1 {
			// Check if all have same body hash (and no etag/vary)
			identical := true
			firstHash := ""
			totalTime := int64(0)
			
			for i, req := range reqs {
				totalTime += req.Duration.Milliseconds()
				if req.HasEtag || req.HasVary {
					identical = false
				}
				if i == 0 {
					firstHash = req.BodyHash
				} else if req.BodyHash != firstHash || firstHash == "" {
					identical = false
				}
			}
			
			stats.RedundantXHR = append(stats.RedundantXHR, DuplicateInfo{
				URL:         url,
				Count:       len(reqs),
				Identical:   identical,
				TotalTimeMs: totalTime,
			})
		}
	}

	return stats, nil
}

// RunMultiple runs the benchmark multiple times and returns the run with median XHR timing
func RunMultiple(name string, page *config.PageConfig, runs int, verbose bool) (*PageStats, error) {
	if runs < 1 {
		runs = 1
	}
	
	var results []*PageStats
	var xhrTotals []time.Duration
	
	for i := 0; i < runs; i++ {
		if verbose {
			fmt.Printf("  Run %d/%d...", i+1, runs)
		}
		stats, err := Run(name, page, false) // Don't be verbose for individual runs
		if err != nil {
			return nil, err
		}
		
		// Calculate total XHR time
		total := time.Duration(0)
		for _, req := range stats.Requests {
			if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
				total += req.Duration
			}
		}
		
		results = append(results, stats)
		xhrTotals = append(xhrTotals, total)
		
		if verbose {
			fmt.Printf(" %s\n", total.Round(time.Millisecond))
		}
	}
	
	// Find median
	indices := make([]int, len(xhrTotals))
	for i := range indices {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool {
		return xhrTotals[indices[i]] < xhrTotals[indices[j]]
	})
	
	medianIdx := indices[len(indices)/2]
	return results[medianIdx], nil
}

// PrintStats outputs page stats to terminal
func PrintStats(stats *PageStats) {
	fmt.Printf("\nPage: %s\n", stats.URL)
	fmt.Println(strings.Repeat("-", 60))
	
	fmt.Printf("\nTiming:\n")
	fmt.Printf("  Time to First Byte: %s\n", stats.TTFB.Round(time.Millisecond))
	fmt.Printf("  Fully Loaded:       %s\n", stats.FullyLoaded.Round(time.Millisecond))
	
	fmt.Printf("\nRequests: %d total\n", stats.TotalRequests)
	
	// By type
	if len(stats.ByType) > 0 {
		fmt.Printf("\nBy Type:\n")
		for typ, count := range stats.ByType {
			fmt.Printf("  %-12s %d\n", typ, count)
		}
	}

	// XHR/Fetch requests with timing
	var xhrRequests []RequestInfo
	for _, req := range stats.Requests {
		if req.ResourceType == "xhr" || req.ResourceType == "fetch" {
			xhrRequests = append(xhrRequests, req)
		}
	}
	if len(xhrRequests) > 0 {
		// Sort by duration descending
		sort.Slice(xhrRequests, func(i, j int) bool {
			return xhrRequests[i].Duration > xhrRequests[j].Duration
		})
		fmt.Printf("\nXHR/Fetch Requests (%d):\n", len(xhrRequests))
		for _, req := range xhrRequests {
			displayURL := truncateURLMiddle(req.URL, 60)
			status := fmt.Sprintf("%d", req.Status)
			fmt.Printf("  %3s %6s  %s\n", status, req.Duration.Round(time.Millisecond), displayURL)
		}
	}
	
	// Redundant XHR (duplicate requests with identical responses)
	var redundant []DuplicateInfo
	for _, dup := range stats.RedundantXHR {
		if !IsDevToolingURL(dup.URL) {
			redundant = append(redundant, dup)
		}
	}
	if len(redundant) > 0 {
		fmt.Printf("\n🔴 Redundant XHR (identical responses, wasted requests):\n")
		for _, dup := range redundant {
			displayURL := truncateURLMiddle(dup.URL, 55)
			status := "identical"
			if !dup.Identical {
				status = "may vary"
			}
			fmt.Printf("  %dx %s [%s, %dms wasted]\n", dup.Count, displayURL, status, dup.TotalTimeMs)
		}
	}
	
	// Other duplicates (excluding redundant XHR already shown)
	var otherDuplicates []string
	redundantURLs := make(map[string]bool)
	for _, r := range stats.RedundantXHR {
		redundantURLs[r.URL] = true
	}
	for url, count := range stats.Duplicates {
		if !IsDevToolingURL(url) && count > 1 && !redundantURLs[url] {
			otherDuplicates = append(otherDuplicates, url)
		}
	}
	if len(otherDuplicates) > 0 {
		fmt.Printf("\n⚠ Other Duplicate Requests:\n")
		for _, url := range otherDuplicates {
			count := stats.Duplicates[url]
			fmt.Printf("  %dx %s\n", count, truncateURLMiddle(url, 60))
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

func extractOrigin(url string) string {
	// Get scheme + host (+ port if present)
	start := strings.Index(url, "://")
	if start == -1 {
		return url
	}
	rest := url[start+3:]
	end := strings.Index(rest, "/")
	if end == -1 {
		return url
	}
	return url[:start+3+end]
}

// IsDevToolingURL returns true for URLs that are dev server tooling, not app code
func IsDevToolingURL(url string) bool {
	devPatterns := []string{
		"@vite",
		"@react-refresh",
		"@fs/",
		"node_modules/.vite",
		"__vite_ping",
		"hot-update",
		".hot-update.",
	}
	for _, pattern := range devPatterns {
		if strings.Contains(url, pattern) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// truncateURLMiddle keeps the start and end, truncates the middle
// For http://vendor-api.localhost:8000/v3/app/380BNB1ocintPMudu.../channels
// Shows: http://vendor-api...8000/v3/app/.../channels
func truncateURLMiddle(url string, maxLen int) string {
	if len(url) <= maxLen {
		return url
	}
	
	// Find where path starts
	pathStart := strings.Index(url, "://")
	if pathStart != -1 {
		pathStart = strings.Index(url[pathStart+3:], "/")
		if pathStart != -1 {
			pathStart += len(url[:strings.Index(url, "://")+3])
		}
	}
	
	// Keep first ~20 chars and last ~35 chars of path
	keepStart := 25
	keepEnd := maxLen - keepStart - 3 // 3 for "..."
	if keepEnd < 20 {
		keepEnd = 20
	}
	
	if pathStart > 0 && pathStart < len(url) {
		// Truncate domain if needed, keep more of path
		path := url[pathStart:]
		if len(path) <= keepEnd+10 {
			// Path is short, truncate domain instead
			domain := url[:pathStart]
			if len(domain) > keepStart {
				domain = domain[:15] + "..." + domain[len(domain)-10:]
			}
			return domain + path
		}
	}
	
	return url[:keepStart] + "..." + url[len(url)-keepEnd:]
}
