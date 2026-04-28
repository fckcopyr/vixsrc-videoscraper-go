package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	tokenRegex     = regexp.MustCompile(`'token':\s*'(\w+)'`)
	expiresRegex   = regexp.MustCompile(`'expires':\s*'(\d+)'`)
	serverURLRegex = regexp.MustCompile(`url:\s*'([^']+)'`)
	keyURIRegex    = regexp.MustCompile(`URI=(?:"([^"]+)"|'([^']+)'|([^\s,]+))`)

	httpClient *http.Client
)

type VixCloudPage struct {
	Version string `json:"version"`
}

type ManifestInfo struct {
	URL     string
	Referer string
}

func makeRequest(ctx context.Context, reqUrl string, headers map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", reqUrl, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", USER_AGENT)
	req.Header.Set("Accept-Language", "it,en-US;q=0.9,en;q=0.8")
	req.Header.Set("DNT", "1")
	req.Header.Set("Sec-GPC", "1")
	req.Header.Set("Connection", "keep-alive")

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("Errore HTTP %d per l'URL: %s", resp.StatusCode, reqUrl)
	}

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return "", fmt.Errorf("errore apertura gzip reader: %w", err)
		}
		defer gr.Close()
		reader = gr
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func makeRequestRaw(ctx context.Context, reqUrl string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", reqUrl, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", USER_AGENT)
	req.Header.Set("Accept-Language", "it,en-US;q=0.9,en;q=0.8")
	req.Header.Set("DNT", "1")
	req.Header.Set("Sec-GPC", "1")
	req.Header.Set("Connection", "keep-alive")

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	return httpClient.Do(req)
}

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

func rewriteMainManifest(manifestContent, baseURL string) string {
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
				result[job.index] = processMainManifestLine(job.line, baseURL)
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

func processMainManifestLine(line, baseURL string) string {
	line = strings.TrimSpace(line)

	if strings.HasPrefix(line, "#EXT-X-KEY:") || strings.HasPrefix(line, "#EXT-X-MEDIA") {
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
					proxiedURI := "/api/v1/vixcloud/secondary?url=" + url.QueryEscape(fullURI)
					return fmt.Sprintf(`URI="%s"`, proxiedURI)
				}
			}
			return match
		})
	} else if line != "" && !strings.HasPrefix(line, "#") {
		fullURL := resolveURL(line, baseURL)
		return "/api/v1/vixcloud/secondary?url=" + url.QueryEscape(fullURL)
	}

	return line
}

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

func processSecondaryManifestLine(line, baseURL string) string {
	line = strings.TrimSpace(line)

	if strings.HasPrefix(line, "#EXT-X-KEY:") {
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
		fullURL := resolveURL(line, baseURL)
		return NGINX_PROXY_BASE + fullURL
	}

	return line
}

func getVixCloudVersion(ctx context.Context, siteURL string) (string, error) {
	headers := map[string]string{
		"Referer":        siteURL + "/",
		"Origin":         siteURL,
		"Accept":         "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Sec-Fetch-Dest": "document",
		"Sec-Fetch-Mode": "navigate",
		"Sec-Fetch-Site": "none",
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

func extractVixCloudManifest(ctx context.Context, inputURL string) (*ManifestInfo, error) {

	if strings.Contains(inputURL, "iframe") {
		siteURL := strings.Split(inputURL, "/iframe")[0]

		version, err := getVixCloudVersion(ctx, siteURL)
		if err != nil {
			return nil, err
		}

		headers := map[string]string{
			"x-inertia":         "true",
			"x-inertia-version": version,
			"Accept":            "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"Referer":           siteURL + "/",
			"Sec-Fetch-Dest":    "document",
			"Sec-Fetch-Mode":    "navigate",
			"Sec-Fetch-Site":    "same-origin",
		}

		iframeResponse, err := makeRequest(ctx, inputURL, headers)
		if err != nil {
			return nil, err
		}

		doc, err := goquery.NewDocumentFromReader(strings.NewReader(iframeResponse))
		if err != nil {
			return nil, err
		}

		iframeSrc, exists := doc.Find("iframe").Attr("src")
		if !exists {
			return nil, fmt.Errorf("iframe not found")
		}

		return extractVixCloudManifest(ctx, iframeSrc)
	}

	if strings.Contains(inputURL, "/movie/") || strings.Contains(inputURL, "/tv/") {
		parsedInput, _ := url.Parse(inputURL)
		parts := strings.Split(strings.TrimRight(parsedInput.Path, "/"), "/")

		var apiURL string
		if strings.Contains(inputURL, "/tv/") {
			if len(parts) < 5 {
				return nil, fmt.Errorf("tv URL must include id, season and episode: /tv/{id}/{season}/{episode}")
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
			"Accept":         "application/json, text/plain, */*",
			"Referer":        fmt.Sprintf("https://vixsrc.to%s", parsedInput.Path),
			"Origin":         "https://vixsrc.to",
			"Sec-Fetch-Dest": "empty",
			"Sec-Fetch-Mode": "cors",
			"Sec-Fetch-Site": "same-origin",
		})
		if err != nil {
			return nil, err
		}

		var result struct {
			Src string `json:"src"`
		}
		if err := json.Unmarshal([]byte(apiResponse), &result); err != nil {
			return nil, fmt.Errorf("failed to parse API response: %w", err)
		}
		if result.Src == "" {
			return nil, fmt.Errorf("empty src in API response")
		}

		embedURL := "https://vixsrc.to" + result.Src

		return extractVixCloudManifest(ctx, embedURL)
	}

	if strings.Contains(inputURL, "/embed/") {
		parsedURL, err := url.Parse(inputURL)
		if err != nil {
			return nil, err
		}

		query := parsedURL.Query()
		canPlayFHD := query.Get("canPlayFHD")
		lang := query.Get("lang")

		embedHeaders := map[string]string{
			"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
			"Referer":                   "https://vixsrc.to/",
			"Sec-Fetch-Dest":            "iframe",
			"Sec-Fetch-Mode":            "navigate",
			"Sec-Fetch-Site":            "same-origin",
			"Upgrade-Insecure-Requests": "1",
			"Priority":                  "u=4",
		}

		embedPageContent, err := makeRequest(ctx, inputURL, embedHeaders)
		if err != nil {
			return nil, fmt.Errorf("errore nel caricare la pagina embed: %w", err)
		}

		tokenMatches := tokenRegex.FindStringSubmatch(embedPageContent)
		if len(tokenMatches) < 2 {
			return nil, fmt.Errorf("token not found in embed page")
		}
		token := tokenMatches[1]

		expiresMatches := expiresRegex.FindStringSubmatch(embedPageContent)
		if len(expiresMatches) < 2 {
			return nil, fmt.Errorf("expires not found in embed page")
		}
		expires := expiresMatches[1]

		serverMatches := serverURLRegex.FindStringSubmatch(embedPageContent)
		if len(serverMatches) < 2 {
			return nil, fmt.Errorf("server URL not found in embed page")
		}
		serverURL := serverMatches[1]

		var manifestURL string
		if strings.Contains(serverURL, "?b=1") {
			manifestURL = fmt.Sprintf("%s&token=%s&expires=%s", serverURL, token, expires)
		} else {
			manifestURL = fmt.Sprintf("%s?token=%s&expires=%s", serverURL, token, expires)
		}

		if canPlayFHD == "1" || strings.Contains(embedPageContent, "window.canPlayFHD = true") {
			manifestURL += "&h=1"
		}
		if lang != "" {
			manifestURL += "&lang=" + url.QueryEscape(lang)
		}

		return &ManifestInfo{
			URL:     manifestURL,
			Referer: inputURL,
		}, nil
	}

	return nil, fmt.Errorf("unsupported URL format: %s", inputURL)
}

func getManifest(c *gin.Context) {
	inputURL := c.Query("url")
	if inputURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing URL parameter"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), REQUEST_TIMEOUT)
	defer cancel()

	info, err := extractVixCloudManifest(ctx, inputURL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	manifestHeaders := map[string]string{
		"Accept":          "*/*",
		"Accept-Encoding": "gzip, deflate, br, zstd",
		"Cache-Control":   "no-cache",
		"Pragma":          "no-cache",
		"Referer":         info.Referer,
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "same-origin",
		"TE":              "trailers",
	}

	manifestContent, err := makeRequest(ctx, info.URL, manifestHeaders)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	rewrittenManifest := rewriteMainManifest(manifestContent, info.URL)

	c.Header("Content-Type", "application/vnd.apple.mpegurl")
	c.String(http.StatusOK, rewrittenManifest)
}

func getSecondaryManifest(c *gin.Context) {
	targetURL := c.Query("url")
	if targetURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing URL parameter"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), REQUEST_TIMEOUT)
	defer cancel()

	headers := map[string]string{
		"Accept":         "*/*",
		"Referer":        "https://vixsrc.to/",
		"Origin":         "https://vixsrc.to",
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Site": "cross-site",
	}

	manifestContent, err := makeRequest(ctx, targetURL, headers)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	rewrittenManifest := rewriteSecondaryManifest(manifestContent, targetURL)

	c.Header("Content-Type", "application/vnd.apple.mpegurl")
	c.String(http.StatusOK, rewrittenManifest)
}

// proxyGeneric è l'equivalente Go del location /proxy/ nginx:
// riceve ?url=... e fa da proxy trasparente verso quella risorsa tramite il client già configurato con WARP.
func proxyGeneric(c *gin.Context) {
	targetURL := c.Query("url")
	if targetURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing url parameter"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), REQUEST_TIMEOUT)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	req.Header.Set("User-Agent", USER_AGENT)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "it,en-US;q=0.9,en;q=0.8")
	req.Header.Set("Origin", "https://vixsrc.to")
	req.Header.Set("Referer", "https://vixsrc.to/")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("DNT", "1")
	req.Header.Set("Sec-GPC", "1")

	resp, err := httpClient.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	lower := strings.ToLower(targetURL)
	if ct == "" {
		switch {
		case strings.Contains(lower, ".m3u8"):
			ct = "application/vnd.apple.mpegurl"
		case strings.Contains(lower, ".ts"):
			ct = "video/mp2t"
		case strings.Contains(lower, ".vtt"):
			ct = "text/vtt"
		default:
			ct = "application/octet-stream"
		}
	}

	c.Header("Content-Type", ct)
	c.Header("Access-Control-Allow-Origin", "*")
	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}

// proxyEncKey fa da proxy verso /storage/enc.key su vixsrc.to
func proxyEncKey(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), REQUEST_TIMEOUT)
	defer cancel()

	resp, err := makeRequestRaw(ctx, "https://vixsrc.to/storage/enc.key", map[string]string{
		"Accept":         "*/*",
		"Referer":        "https://vixsrc.to/",
		"Origin":         "https://vixsrc.to",
		"Sec-Fetch-Dest": "empty",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Site": "same-origin",
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	c.Header("Content-Type", "application/octet-stream")
	c.Header("Access-Control-Allow-Origin", "*")
	c.Status(resp.StatusCode)
	io.Copy(c.Writer, resp.Body)
}

func init() {
	jar, _ := cookiejar.New(nil)

	warpProxyURL, _ := url.Parse("http://warp:1080")

	httpClient = &http.Client{
		Timeout: REQUEST_TIMEOUT,
		Jar:     jar,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			Proxy:               http.ProxyURL(warpProxyURL),
		},
	}
}

func main() {
	_ = godotenv.Load()

	NGINX_PROXY_BASE = os.Getenv("NGINX_PROXY_BASE")
	USER_AGENT = os.Getenv("USER_AGENT")
	if USER_AGENT == "" {
		USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64; rv:149.0) Gecko/20100101 Firefox/149.0"
	}

	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	r.Use(gin.Recovery())

	config := cors.DefaultConfig()
	config.AllowAllOrigins = true
	r.Use(cors.New(config))

	// Endpoint VixCloud originali
	r.GET("/api/v1/vixcloud/manifest", getManifest)
	r.GET("/api/v1/vixcloud/secondary", getSecondaryManifest)

	// Nuovi endpoint proxy sulla root (equivalenti nginx)
	r.GET("/proxy", proxyGeneric)
	r.GET("/storage/enc.key", proxyEncKey)

	r.GET("/debug/ip", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), REQUEST_TIMEOUT)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=json", nil)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		c.Data(http.StatusOK, "application/json", body)
	})

	fmt.Println("Server starting on :5000")
	if err := r.Run(":5000"); err != nil {
		panic(err)
	}
}
