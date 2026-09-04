package webfetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
)

func TestFetch_HTMLPage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>Hello</body></html>"))
	}))
	defer ts.Close()

	res, err := Fetch(ts.URL, "", nil, 0, 0)
	require.NoError(t, err)
	got := res.(fetchResult)
	assert.Equal(t, ts.URL, got.URL)
	assert.Equal(t, http.StatusOK, got.StatusCode)
	assert.Contains(t, got.Body, "Hello")
	assert.Equal(t, "text/html", got.Headers["Content-Type"])
	assert.True(t, got.ConvertedFromHTML)
	assert.False(t, got.Truncated)
	assert.Equal(t, len(got.Body), got.ContentBytes)
}

func TestFetch_HTMLExtractionOmitsNoiseAndResolvesLinks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html>
<head>
  <title>Guide</title>
  <style>body { display: none }</style>
  <script>const secret = "script-value";</script>
</head>
<body>
  <h1>Installation</h1>
  <p>Read <a href="/docs/start">the documentation</a> first.</p>
  <pre><code>make test</code></pre>
</body>
</html>`))
	}))
	defer ts.Close()

	res, err := Fetch(ts.URL, "", nil, 0, 0)
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.True(t, got.ConvertedFromHTML)
	assert.False(t, got.Truncated)
	assert.Contains(t, got.Body, "# Guide")
	assert.Contains(t, got.Body, "# Installation")
	assert.Contains(t, got.Body, "[the documentation]("+ts.URL+"/docs/start)")
	assert.Contains(t, got.Body, "```\nmake test\n```")
	assert.NotContains(t, got.Body, "script-value")
	assert.NotContains(t, got.Body, "display: none")
	assert.Equal(t, len(got.Body), got.ContentBytes)
}

func TestFetch_NonHTMLContentIsNotConverted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("<not-html> plain text"))
	}))
	defer ts.Close()

	res, err := Fetch(ts.URL, "", nil, 0, 0)
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.False(t, got.ConvertedFromHTML)
	assert.Equal(t, "<not-html> plain text", got.Body)
	assert.Equal(t, len(got.Body), got.ContentBytes)
	assert.False(t, got.Truncated)
}

func TestFetch_DownloadTruncation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("x", 32)))
	}))
	defer ts.Close()

	res, err := FetchWithOptions(ts.URL, "", nil, 0, 0, Options{
		MaxDownloadBytes: 8,
		MaxOutputBytes:   128,
	})
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.True(t, got.DownloadTruncated)
	assert.True(t, got.Truncated)
	assert.Equal(t, 8, got.ContentBytes)
	assert.Equal(t, 8, strings.Count(got.Body, "x"))
	assert.Contains(t, got.Body, "download truncated at 8 bytes")
}

func TestFetch_OutputTruncation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("y", 64)))
	}))
	defer ts.Close()

	res, err := FetchWithOptions(ts.URL, "", nil, 0, 0, Options{
		MaxDownloadBytes: 1024,
		MaxOutputBytes:   60,
	})
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.False(t, got.DownloadTruncated)
	assert.True(t, got.Truncated)
	assert.Equal(t, 64, got.ContentBytes)
	assert.Equal(t, 60, len(got.Body))
	assert.Contains(t, got.Body, "content truncated at 60 of 64 bytes")
	assert.Equal(t, 11, strings.Count(got.Body, "y"))
}

func TestFetch_OutputTruncationRespectsRuneBoundary(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(strings.Repeat("中文", 100)))
	}))
	defer ts.Close()

	res, err := FetchWithOptions(ts.URL, "", nil, 0, 0, Options{
		MaxDownloadBytes: 1024,
		MaxOutputBytes:   70,
	})
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.True(t, got.Truncated)
	assert.Equal(t, 600, got.ContentBytes)
	assert.LessOrEqual(t, len(got.Body), 70)
	assert.Contains(t, got.Body, "中")
	assert.Contains(t, got.Body, "content truncated at 70 of 600 bytes")
}

func TestOptions_Defaults(t *testing.T) {
	got := Options{MaxDownloadBytes: -1, MaxOutputBytes: -2}.resolve()
	assert.Equal(t, 5<<20, got.MaxDownloadBytes)
	assert.Equal(t, 100<<10, got.MaxOutputBytes)
}

func TestHTMLToMarkdown_ListsImagesAndTables(t *testing.T) {
	base := &url.URL{Scheme: "https", Host: "example.com"}
	got, err := htmlToMarkdown([]byte(`<body>
<ol><li>first</li><li>second</li></ol>
<img src="/chart.png" alt="Results chart">
<table><tr><th>Name</th><th>Value</th></tr><tr><td>alpha</td><td>42</td></tr></table>
</body>`), "text/html", base)
	require.NoError(t, err)

	assert.Contains(t, got, "1. first")
	assert.Contains(t, got, "2. second")
	assert.Contains(t, got, "![Results chart](https://example.com/chart.png)")
	assert.Contains(t, got, "| Name | Value |")
	assert.Contains(t, got, "| --- | --- |")
	assert.Contains(t, got, "| alpha | 42 |")
}

func TestFetch_FollowsRedirect(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("final"))
	}))
	defer final.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirect.Close()

	res, err := Fetch(redirect.URL, "", nil, 0, 0)
	require.NoError(t, err)
	got := res.(fetchResult)
	assert.Equal(t, final.URL, got.URL)
	assert.Equal(t, http.StatusOK, got.StatusCode)
	assert.Contains(t, got.Body, "final")
}

func TestFetch_RedirectWithinCap(t *testing.T) {
	// A 3-hop chain (a -> b -> c -> 200) stays under the cap and is followed.
	c := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("final-c"))
	}))
	defer c.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, c.URL, http.StatusFound)
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL, http.StatusFound)
	}))
	defer a.Close()

	res, err := Fetch(a.URL, "", nil, 0, 5)
	require.NoError(t, err)
	got := res.(fetchResult)
	assert.Equal(t, c.URL, got.URL)
	assert.Equal(t, http.StatusOK, got.StatusCode)
	assert.Contains(t, got.Body, "final-c")
}

func TestFetch_RedirectCapExceeded(t *testing.T) {
	// A self-redirect loop never terminates; the cap must stop it and surface
	// the last redirect location so the agent can react.
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.RequestURI(), http.StatusFound)
	}))
	defer loop.Close()

	_, err := Fetch(loop.URL, "", nil, 0, 3)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped after 3 redirects")
	assert.Contains(t, err.Error(), "last redirect location")
	assert.Contains(t, err.Error(), loop.URL)
}

func TestFetch_DefaultMaxRedirects(t *testing.T) {
	// A zero/negative maxRedirects falls back to the default (10); a loop still
	// terminates at that cap.
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.RequestURI(), http.StatusFound)
	}))
	defer loop.Close()

	_, err := Fetch(loop.URL, "", nil, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stopped after 10 redirects")
}

func TestFetch_CustomMethodAndHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	res, err := Fetch(ts.URL, http.MethodPost, map[string]string{"Content-Type": "application/json"}, 0, 0)
	require.NoError(t, err)
	got := res.(fetchResult)
	assert.Equal(t, http.StatusAccepted, got.StatusCode)
}

func TestFetch_Timeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	_, err := Fetch(ts.URL, "", nil, 1*time.Millisecond, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

func TestFetch_InvalidURL(t *testing.T) {
	_, err := Fetch("://bad-url", "", nil, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid url")
}

func TestFetch_EmptyURL(t *testing.T) {
	_, err := Fetch("", "", nil, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "url is empty")
}

func TestRegisterAll_Enabled(t *testing.T) {
	r := tool.NewRegistry()
	RegisterAll(r, config.WebfetchConfig{Enabled: true, Timeout: 30 * time.Second})

	spec, ok := r.Get(Name)
	require.True(t, ok)
	assert.Equal(t, Name, spec.Name)

	args, err := json.Marshal(fetchArgs{URL: "://invalid-url"})
	require.NoError(t, err)

	// Invalid URL exercises arg decoding without network access.
	_, err = spec.Execute(context.Background(), args)
	assert.Error(t, err)
}

func TestRegisterAll_Disabled(t *testing.T) {
	r := tool.NewRegistry()
	RegisterAll(r, config.WebfetchConfig{Enabled: false})
	_, ok := r.Get(Name)
	assert.False(t, ok)
}

// TestRegisterAll_DescriptionDeclaresErrorRecovery pins the existing "description
// guides error recovery" requirement: the description tells the model to read the
// returned status/Location and retry, and the url parameter requires a scheme.
func TestRegisterAll_DescriptionDeclaresErrorRecovery(t *testing.T) {
	r := tool.NewRegistry()
	RegisterAll(r, config.WebfetchConfig{Enabled: true})

	spec, ok := r.Get(Name)
	require.True(t, ok)
	assert.Contains(t, spec.Description, "Location")
	assert.Contains(t, spec.Description, "retry")
	assert.Contains(t, spec.Description, "converted_from_html")
	assert.Contains(t, spec.Description, "truncated")
	assert.Contains(t, spec.Description, "objective")
	assert.Contains(t, string(spec.ParametersJSON), `"objective"`)
	// url parameter description makes the scheme explicit.
	assert.Contains(t, string(spec.ParametersJSON), "Absolute http(s) URL")
}

func TestRegisterAll_PassesContentLimits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("z", 32)))
	}))
	defer ts.Close()

	r := tool.NewRegistry()
	RegisterAll(r, config.WebfetchConfig{
		Enabled:          true,
		MaxDownloadBytes: 8,
		MaxOutputBytes:   128,
	})
	spec, ok := r.Get(Name)
	require.True(t, ok)

	args, err := json.Marshal(fetchArgs{URL: ts.URL})
	require.NoError(t, err)
	res, err := spec.Execute(context.Background(), args)
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.True(t, got.DownloadTruncated)
	assert.Equal(t, 8, got.ContentBytes)
	assert.Equal(t, 8, strings.Count(got.Body, "z"))
}
