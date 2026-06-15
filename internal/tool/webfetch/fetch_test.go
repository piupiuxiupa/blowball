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

	res, err := Fetch(ts.URL, "", nil, 0)
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

	res, err := Fetch(redirect.URL, "", nil, 0)
	require.NoError(t, err)
	got := res.(fetchResult)
	assert.Equal(t, final.URL, got.URL)
	assert.Equal(t, http.StatusOK, got.StatusCode)
	assert.Contains(t, got.Body, "final")
}

func TestFetch_CustomMethodAndHeaders(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	res, err := Fetch(ts.URL, http.MethodPost, map[string]string{"Content-Type": "application/json"}, 0)
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

	_, err := Fetch(ts.URL, "", nil, 1*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "timeout")
}

func TestFetch_InvalidURL(t *testing.T) {
	_, err := Fetch("://bad-url", "", nil, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid url")
}

func TestFetch_EmptyURL(t *testing.T) {
	_, err := Fetch("", "", nil, 0)
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
