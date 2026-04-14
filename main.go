package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

var (
	NGINX_PROXY_BASE string
	USER_AGENT       string
)

const (
	REQUEST_TIMEOUT = 30 * time.Second
)

var (
	// Pre-compiled regex patterns for better performance
	tokenRegex     = regexp.MustCompile(`'token':\s*'(\w+)'`)
	expiresRegex   = regexp.MustCompile(`'expires':\s*'(\d+)'`)
	serverURLRegex = regexp.MustCompile(`url:\s*'([^']+)'`)
	keyURIRegex    = regexp.MustCompile(`URI=(?:"([^"]+)"|'([^']+)'|([^\s,]+))`)

	// HTTP client pool
	httpClient = &http.Client{
		Timeout: REQUEST_TIMEOUT,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
)

type VixCloudData struct {
	Version string `json:"version"`
}

type VixCloudPage struct {
	Version string `json:"version"`
}

// Async HTTP request with context
func makeRequest(ctx context.Context, url string, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}

	// Set headers
	req.Header.Set("User-Agent", USER_AGENT)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// Resolve relative URLs to absolute
func resolveURL(targetURL, baseURL string) string {
	if strings.HasPrefix(targetURL, "http") {
		return targetURL
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		return targetURL
	}

	if strings.HasPrefix(targetURL, "/") {
		return fmt.Sprintf("%s://%s%s", base.Scheme, base.Host, targetURL)
	}

	resolved, err := base.Parse(targetURL)
	if err != nil {
		return targetURL
	}

	return resolved.String()
}

// Rewrite manifest URLs for MAIN playlist (points to secondary playlist endpoint)
func rewriteMainManifest(manifestContent, baseURL string) string {
	lines := strings.Split(manifestContent, "\n")
	result := make([]string, len(lines))

	// Use worker pool for parallel processing
	const numWorkers = 4
	jobs := make(chan struct {
		index int
		line  string
	}, len(lines))

	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				result[job.index] = processMainManifestLine(job.line, baseURL)
			}
		}()
	}

	// Send jobs
	go func() {
		defer close(jobs)
		for i, line := range lines {
			jobs <- struct {
				index int
				line  string
			}{i, line}
		}
	}()

	wg.Wait()

	return strings.Join(result, "\n")
}

// Process single line for MAIN manifest
func processMainManifestLine(line, baseURL string) string {
	line = strings.TrimSpace(line)

	if strings.HasPrefix(line, "#EXT-X-KEY:") || strings.HasPrefix(line, "#EXT-X-MEDIA") {
		// Rewrite URI in metadata lines to point to secondary playlist endpoint
		return keyURIRegex.ReplaceAllStringFunc(line, func(match string) string {
			submatches := keyURIRegex.FindStringSubmatch(match)
			if len(submatches) >= 4 {
				var originalURI string
				if submatches[1] != "" {
					originalURI = submatches[1]
				} else if submatches[2] != "" {
					originalURI = submatches[2]
				} else if submatches[3] != "" {
					originalURI = submatches[3]
				}

				if originalURI != "" {
					fullURI := resolveURL(originalURI, baseURL)
					// Point to secondary playlist endpoint instead of nginx directly
					proxiedURI := "/api/v1/vixcloud/secondary?url=" + url.QueryEscape(fullURI)
					return fmt.Sprintf(`URI="%s"`, proxiedURI)
				}
			}
			return match
		})
	} else if line != "" && !strings.HasPrefix(line, "#") {
		// Rewrite segment/playlist URLs to point to secondary playlist endpoint
		fullURL := resolveURL(line, baseURL)
		return "/api/v1/vixcloud/secondary?url=" + url.QueryEscape(fullURL)
	}

	return line
}

// Rewrite SECONDARY playlist URLs (points segments to nginx proxy)
func rewriteSecondaryManifest(manifestContent, baseURL string) string {
	lines := strings.Split(manifestContent, "\n")
	result := make([]string, len(lines))

	const numWorkers = 4
	jobs := make(chan struct {
		index int
		line  string
	}, len(lines))

	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				result[job.index] = processSecondaryManifestLine(job.line, baseURL)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for i, line := range lines {
			jobs <- struct {
				index int
				line  string
			}{i, line}
		}
	}()

	wg.Wait()

	return strings.Join(result, "\n")
}

// Process single line for SECONDARY manifest
func processSecondaryManifestLine(line, baseURL string) string {
	line = strings.TrimSpace(line)

	if strings.HasPrefix(line, "#EXT-X-KEY:") {
		// Rewrite key URIs to point to nginx proxy
		return keyURIRegex.ReplaceAllStringFunc(line, func(match string) string {
			submatches := keyURIRegex.FindStringSubmatch(match)
			if len(submatches) >= 4 {
				var originalURI string
				if submatches[1] != "" {
					originalURI = submatches[1]
				} else if submatches[2] != "" {
					originalURI = submatches[2]
				} else if submatches[3] != "" {
					originalURI = submatches[3]
				}

				if originalURI != "" {
					fullURI := resolveURL(originalURI, baseURL)
					proxiedURI := NGINX_PROXY_BASE + fullURI
					return fmt.Sprintf(`URI="%s"`, proxiedURI)
				}
			}
			return match
		})
	} else if line != "" && !strings.HasPrefix(line, "#") {
		// Rewrite segment URLs (.ts files) to point to nginx proxy
		fullURL := resolveURL(line, baseURL)
		return NGINX_PROXY_BASE + fullURL
	}

	return line
}

// Extract manifest URL from VixCloud page
func extractVixCloudManifest(ctx context.Context, inputURL string) (string, error) {
	var response string

	if strings.Contains(inputURL, "iframe") {
		siteURL := strings.Split(inputURL, "/iframe")[0]

		var version string
		var wg sync.WaitGroup
		var versionErr error

		wg.Add(1)
		go func() {
			defer wg.Done()
			version, versionErr = getVixCloudVersion(ctx, siteURL)
		}()

		wg.Wait()

		if versionErr != nil {
			return "", versionErr
		}

		headers := map[string]string{
			"x-inertia":         "true",
			"x-inertia-version": version,
		}

		iframeResponse, err := makeRequest(ctx, inputURL, headers)
		if err != nil {
			return "", err
		}

		doc, err := goquery.NewDocumentFromReader(strings.NewReader(iframeResponse))
		if err != nil {
			return "", err
		}

		iframe, exists := doc.Find("iframe").Attr("src")
		if !exists {
			return "", fmt.Errorf("iframe not found")
		}

		response, err = makeRequest(ctx, iframe, headers)
		if err != nil {
			return "", err
		}

	} else if strings.Contains(inputURL, "/movie/") || strings.Contains(inputURL, "/tv/") {
		parsedInput, _ := url.Parse(inputURL)
		parts := strings.Split(strings.TrimRight(parsedInput.Path, "/"), "/")

		var apiURL string
		if strings.Contains(inputURL, "/tv/") {
			if len(parts) < 5 {
				return "", fmt.Errorf("tv URL must include id, season and episode: /tv/{id}/{season}/{episode}")
			}
			id := parts[2]
			season := parts[3]
			episode := parts[4]
			apiURL = fmt.Sprintf("https://vixsrc.to/api/tv/%s/%s/%s", id, season, episode)
		} else {
			id := parts[len(parts)-1]
			apiURL = fmt.Sprintf("https://vixsrc.to/api/movie/%s", id)
		}

		apiResponse, err := makeRequest(ctx, apiURL, map[string]string{
			"Accept":  "application/json",
			"Referer": "https://vixsrc.to/",
		})
		if err != nil {
			return "", err
		}

		var result struct {
			Src string `json:"src"`
		}
		if err := json.Unmarshal([]byte(apiResponse), &result); err != nil {
			return "", fmt.Errorf("failed to parse API response: %w", err)
		}
		if result.Src == "" {
			return "", fmt.Errorf("empty src in API response")
		}

		embedURL := "https://vixsrc.to" + result.Src
		return extractVixCloudManifest(ctx, embedURL)

	} else if strings.Contains(inputURL, "embed") {
		parsedURL, err := url.Parse(inputURL)
		if err != nil {
			return "", err
		}

		query := parsedURL.Query()
		urlToken := query.Get("token")
		urlExpires := query.Get("expires")
		canPlayFHD := query.Get("canPlayFHD")

		if urlToken != "" && urlExpires != "" {
			response, err = makeRequest(ctx, inputURL, nil)
			if err != nil {
				return "", err
			}

			if matches := serverURLRegex.FindStringSubmatch(response); len(matches) > 1 {
				serverURL := matches[1]

				var manifestURL string
				if strings.Contains(serverURL, "?b=1") {
					manifestURL = fmt.Sprintf("%s&token=%s&expires=%s", serverURL, urlToken, urlExpires)
				} else {
					manifestURL = fmt.Sprintf("%s?token=%s&expires=%s", serverURL, urlToken, urlExpires)
				}

				if canPlayFHD == "1" {
					manifestURL += "&h=1"
				}

				return manifestURL, nil
			}
			return "", fmt.Errorf("server URL not found in embed page")
		}

		response, err = makeRequest(ctx, inputURL, nil)
		if err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("unsupported URL format")
	}

	var token, expires, serverURL string
	var tokenErr, expiresErr, serverErr error
	var wg sync.WaitGroup

	wg.Add(3)

	go func() {
		defer wg.Done()
		if matches := tokenRegex.FindStringSubmatch(response); len(matches) > 1 {
			token = matches[1]
		} else {
			tokenErr = fmt.Errorf("token not found")
		}
	}()

	go func() {
		defer wg.Done()
		if matches := expiresRegex.FindStringSubmatch(response); len(matches) > 1 {
			expires = matches[1]
		} else {
			expiresErr = fmt.Errorf("expires not found")
		}
	}()

	go func() {
		defer wg.Done()
		if matches := serverURLRegex.FindStringSubmatch(response); len(matches) > 1 {
			serverURL = matches[1]
		} else {
			serverErr = fmt.Errorf("server URL not found")
		}
	}()

	wg.Wait()

	if tokenErr != nil {
		return "", tokenErr
	}
	if expiresErr != nil {
		return "", expiresErr
	}
	if serverErr != nil {
		return "", serverErr
	}

	var manifestURL string
	if strings.Contains(serverURL, "?b=1") {
		manifestURL = fmt.Sprintf("%s&token=%s&expires=%s", serverURL, token, expires)
	} else {
		manifestURL = fmt.Sprintf("%s?token=%s&expires=%s", serverURL, token, expires)
	}

	if strings.Contains(response, "window.canPlayFHD = true") {
		manifestURL += "&h=1"
	}

	return manifestURL, nil
}

// Get VixCloud version
func getVixCloudVersion(ctx context.Context, siteURL string) (string, error) {
	headers := map[string]string{
		"Referer": siteURL + "/",
		"Origin":  siteURL,
	}

	response, err := makeRequest(ctx, siteURL+"/request-a-title", headers)
	if err != nil {
		return "", err
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(response))
	if err != nil {
		return "", err
	}

	dataPage, exists := doc.Find("div#app").Attr("data-page")
	if !exists {
		return "", fmt.Errorf("data-page not found")
	}

	var pageData VixCloudPage
	if err := json.Unmarshal([]byte(dataPage), &pageData); err != nil {
		return "", err
	}

	return pageData.Version, nil
}

// MAIN manifest handler (processes the first level playlist)
func getManifest(c *gin.Context) {
	inputURL := c.Query("url")
	if inputURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing URL parameter"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), REQUEST_TIMEOUT)
	defer cancel()

	// Extract manifest URL
	manifestURL, err := extractVixCloudManifest(ctx, inputURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Download manifest content
	var manifestContent string
	var manifestErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		headers := map[string]string{"referer": inputURL}
		manifestContent, manifestErr = makeRequest(ctx, manifestURL, headers)
	}()

	wg.Wait()

	if manifestErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": manifestErr.Error()})
		return
	}

	// Rewrite URLs for main manifest (points to secondary endpoint)
	rewrittenManifest := rewriteMainManifest(manifestContent, manifestURL)

	c.Header("Content-Type", "application/vnd.apple.mpegurl")
	c.String(http.StatusOK, rewrittenManifest)
}

// SECONDARY manifest handler (processes playlist with .ts segments)
func getSecondaryManifest(c *gin.Context) {
	targetURL := c.Query("url")
	if targetURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing URL parameter"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), REQUEST_TIMEOUT)
	defer cancel()

	// Download secondary manifest content
	headers := map[string]string{
		"User-Agent": USER_AGENT,
		"Referer":    "https://vixsrc.to/",
		"Origin":     "https://vixsrc.to/",
	}

	manifestContent, err := makeRequest(ctx, targetURL, headers)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Rewrite URLs for secondary manifest (points segments to nginx)
	rewrittenManifest := rewriteSecondaryManifest(manifestContent, targetURL)

	c.Header("Content-Type", "application/vnd.apple.mpegurl")
	c.String(http.StatusOK, rewrittenManifest)
}

func main() {
	// Load .env file if it exists
	_ = godotenv.Load()

	NGINX_PROXY_BASE = os.Getenv("NGINX_PROXY_BASE")
	USER_AGENT = os.Getenv("USER_AGENT")
	if USER_AGENT == "" {
		USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}

	// Set Gin to release mode for better performance
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()

	// Add recovery middleware
	r.Use(gin.Recovery())

	// Configure CORS
	config := cors.DefaultConfig()
	config.AllowAllOrigins = true
	r.Use(cors.New(config))

	// Routes
	r.GET("/api/v1/vixcloud/manifest", getManifest)           // Main playlist
	r.GET("/api/v1/vixcloud/secondary", getSecondaryManifest) // Secondary playlist

	// Start server
	fmt.Println("Server starting on :5000")
	if err := r.Run(":5000"); err != nil {
		panic(err)
	}
}
