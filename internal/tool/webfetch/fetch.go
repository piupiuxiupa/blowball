// Package webfetch implements the webfetch tool: a simple HTTP client that
// fetches external URLs and returns the response as text.
package webfetch

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultTimeout = 30 * time.Second

// defaultMaxRedirects caps how many HTTP redirects webfetch follows. It mirrors
// net/http's standard defaultRedirectLimit so behaviour matches the Go default
// client; a zero or negative maxRedirects falls back to this value.
const defaultMaxRedirects = 10

// fetchResult is the JSON-serializable result returned by Fetch.
type fetchResult struct {
	URL        string            `json:"url"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

// Fetch performs an HTTP request to rawURL using the given method and headers,
// following redirects up to maxRedirects times and honouring the supplied
// timeout. A zero or negative timeout falls back to the default 30 seconds; a
// zero or negative maxRedirects falls back to defaultMaxRedirects (10). When the
// redirect cap is reached the request fails with a "stopped after N redirects"
// error that also carries the last redirect location. The response body is
// returned as a string; non-UTF-8 bytes may produce invalid UTF-8 in the JSON
// output.
func Fetch(rawURL, method string, headers map[string]string, timeout time.Duration, maxRedirects int) (any, error) {
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("webfetch: read body: %w", err)
	}

	resultHeaders := make(map[string]string, len(resp.Header))
	for k, v := range resp.Header {
		if len(v) > 0 {
			resultHeaders[k] = strings.Join(v, ", ")
		}
	}

	return fetchResult{
		URL:        resp.Request.URL.String(),
		StatusCode: resp.StatusCode,
		Headers:    resultHeaders,
		Body:       string(body),
	}, nil
}
