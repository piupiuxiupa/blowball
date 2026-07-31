package webfetch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// url parameter description makes the scheme explicit.
	assert.Contains(t, string(spec.ParametersJSON), "Absolute http(s) URL")
}
