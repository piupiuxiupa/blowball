// Package webfetch implements the webfetch tool: a bounded HTTP client that
// fetches external URLs and returns model-oriented text content.
package webfetch

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const defaultTimeout = 30 * time.Second

// defaultMaxRedirects caps how many HTTP redirects webfetch follows. It mirrors
// net/http's standard defaultRedirectLimit so behaviour matches the Go default
// client; a zero or negative maxRedirects falls back to this value.
const defaultMaxRedirects = 10

const defaultMaxDownloadBytes = 5 << 20

const defaultMaxOutputBytes = 100 << 10

// fetchResult is the JSON-serializable result returned by Fetch.
type fetchResult struct {
	URL               string            `json:"url"`
	StatusCode        int               `json:"status_code"`
	Headers           map[string]string `json:"headers"`
	Body              string            `json:"body"`
	ConvertedFromHTML bool              `json:"converted_from_html"`
	ContentBytes      int               `json:"content_bytes"`
	DownloadTruncated bool              `json:"download_truncated"`
	Truncated         bool              `json:"truncated"`
}

// Options carries the two independent webfetch content bounds. MaxDownloadBytes
// limits bytes retained from the network; MaxOutputBytes limits extracted
// content injected into model context.
type Options struct {
	MaxDownloadBytes int
	MaxOutputBytes   int
}

func (o Options) resolve() Options {
	if o.MaxDownloadBytes <= 0 {
		o.MaxDownloadBytes = defaultMaxDownloadBytes
	}
	if o.MaxOutputBytes <= 0 {
		o.MaxOutputBytes = defaultMaxOutputBytes
	}
	return o
}

// Fetch performs an HTTP request to rawURL using the given method and headers,
// following redirects up to maxRedirects times and honouring the supplied
// timeout. A zero or negative timeout falls back to the default 30 seconds; a
// zero or negative maxRedirects falls back to defaultMaxRedirects (10). When the
// redirect cap is reached the request fails with a "stopped after N redirects"
// error that also carries the last redirect location. HTML is extracted to
// Markdown-oriented text, content is bounded by the default limits, and
// non-UTF-8 bytes in non-HTML responses may render as replacement characters
// in JSON.
func Fetch(rawURL, method string, headers map[string]string, timeout time.Duration, maxRedirects int) (any, error) {
	return FetchWithOptions(rawURL, method, headers, timeout, maxRedirects, Options{})
}

// FetchWithOptions is Fetch with explicit content limits. Zero or negative
// limits resolve to the documented defaults.
func FetchWithOptions(rawURL, method string, headers map[string]string, timeout time.Duration, maxRedirects int, options Options) (any, error) {
	options = options.resolve()
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("webfetch: url is empty")
	}
	if _, err := url.Parse(rawURL); err != nil {
		return nil, fmt.Errorf("webfetch: invalid url: %w", err)
	}
	if method == "" {
		method = http.MethodGet
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxRedirects <= 0 {
		maxRedirects = defaultMaxRedirects
	}

	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("webfetch: create request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// lastLocation captures the most recent redirect target so any failure that
	// occurs after at least one redirect (cap exceeded, loop, mid-redirect TLS
	// error, ...) can surface where the server was trying to send the client.
	var lastLocation string
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			lastLocation = req.URL.String()
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "context deadline exceeded") {
			return nil, fmt.Errorf("webfetch: request timeout: %w", err)
		}
		if lastLocation != "" {
			return nil, fmt.Errorf("webfetch: request failed: %w; last redirect location: %s", err, lastLocation)
		}
		return nil, fmt.Errorf("webfetch: request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read one byte beyond the cap so truncation is detectable without a
	// second network read, then discard the probe byte when the cap was hit.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(options.MaxDownloadBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("webfetch: read body: %w", err)
	}
	downloadTruncated := len(body) > options.MaxDownloadBytes
	if downloadTruncated {
		body = body[:options.MaxDownloadBytes]
	}

	contentType := resp.Header.Get("Content-Type")
	converted := false
	content := string(body)
	if isHTMLResponse(body, contentType) {
		content, err = htmlToMarkdown(body, contentType, resp.Request.URL)
		if err != nil {
			return nil, fmt.Errorf("webfetch: extract html: %w", err)
		}
		converted = true
	}

	contentBytes := len(content)
	outputTruncated := contentBytes > options.MaxOutputBytes
	truncated := downloadTruncated || outputTruncated
	if truncated {
		marker := truncationMarker(options.MaxOutputBytes, downloadTruncated, outputTruncated, options.MaxDownloadBytes, contentBytes)
		content = truncateUTF8(content, options.MaxOutputBytes-len(marker)) + marker
	}

	resultHeaders := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			resultHeaders[k] = strings.Join(v, ", ")
		}
	}

	return fetchResult{
		URL:               resp.Request.URL.String(),
		StatusCode:        resp.StatusCode,
		Headers:           resultHeaders,
		Body:              content,
		ConvertedFromHTML: converted,
		ContentBytes:      contentBytes,
		DownloadTruncated: downloadTruncated,
		Truncated:         truncated,
	}, nil
}

func truncateUTF8(s string, limit int) string {
	if limit < 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cutoff := limit
	for cutoff > 0 && !utf8.RuneStart(s[cutoff]) {
		cutoff--
	}
	return s[:cutoff]
}

func truncationMarker(limit int, download, output bool, maxDownloadBytes, contentBytes int) string {
	reasons := make([]string, 0, 2)
	if download {
		reasons = append(reasons, fmt.Sprintf("download truncated at %d bytes", maxDownloadBytes))
	}
	if output {
		reasons = append(reasons, fmt.Sprintf("content truncated at %d of %d bytes", limit, contentBytes))
	}
	full := "\n\n[webfetch: " + strings.Join(reasons, "; ") + "]\n"
	if len(full) <= limit {
		return full
	}
	switch {
	case limit >= 6:
		return "\n[...]"
	case limit == 5:
		return "[...]"
	case limit == 4:
		return "..."
	case limit == 3:
		return "…"
	case limit == 2:
		return ".."
	case limit == 1:
		return "!"
	default:
		return ""
	}
}
