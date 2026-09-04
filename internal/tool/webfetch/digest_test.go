package webfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
	"github.com/lush/blowball/internal/tool/skill"
)

type promptCall struct {
	System          string
	User            string
	MaxOutputTokens int
}

type fakePromptClient struct {
	mu        sync.Mutex
	calls     []promptCall
	active    int
	maxActive int
	handler   func(call promptCall) (string, error)
}

func (f *fakePromptClient) Prompt(_ context.Context, system, user string, maxOutputTokens int) (string, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxActive {
		f.maxActive = f.active
	}
	f.mu.Unlock()

	out, err := f.handler(promptCall{System: system, User: user, MaxOutputTokens: maxOutputTokens})

	f.mu.Lock()
	f.active--
	f.calls = append(f.calls, promptCall{System: system, User: user, MaxOutputTokens: maxOutputTokens})
	f.mu.Unlock()
	return out, err
}

func (f *fakePromptClient) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakePromptClient) maximumActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func digestTestConfig() config.WebfetchDigestConfig {
	return config.WebfetchDigestConfig{
		Enabled:            true,
		ThresholdBytes:     16,
		SingleShotMaxBytes: 32,
		ChunkBytes:         32,
		MaxChunks:          8,
		Concurrency:        2,
		Timeout:            2 * time.Second,
		ChunkOutputTokens:  100,
		FinalOutputTokens:  200,
	}
}

func plainTextServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFetchWithContext_BelowDigestThresholdMakesNoModelCall(t *testing.T) {
	client := &fakePromptClient{handler: func(promptCall) (string, error) {
		return "unexpected", nil
	}}
	server := plainTextServer(t, "tiny page")
	digester := NewDigester(client, digestTestConfig(), "small-model", nil)

	res, err := FetchWithContext(context.Background(), server.URL, "keep facts", http.MethodGet, nil, time.Second, 1, Options{MaxOutputBytes: 1024}, digester)
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.Equal(t, ProcessingModeDirect, got.ProcessingMode)
	assert.Nil(t, got.Digest)
	assert.Equal(t, "tiny page", got.Body)
	assert.Equal(t, 0, client.count())
}

func TestFetchWithContext_SingleShotDigest(t *testing.T) {
	content := strings.Repeat("s", 24)
	client := &fakePromptClient{handler: func(call promptCall) (string, error) {
		require.Contains(t, call.User, "single objective")
		require.Contains(t, call.User, content)
		return "single digest", nil
	}}
	server := plainTextServer(t, content)
	digester := NewDigester(client, digestTestConfig(), "small-model", nil)

	res, err := FetchWithContext(context.Background(), server.URL, "single objective", http.MethodGet, nil, time.Second, 1, Options{MaxOutputBytes: 1024}, digester)
	require.NoError(t, err)
	got := res.(fetchResult)

	require.NotNil(t, got.Digest)
	assert.Equal(t, ProcessingModeSingleShot, got.ProcessingMode)
	assert.Equal(t, ProcessingModeSingleShot, got.Digest.Mode)
	assert.Equal(t, digestStatusOK, got.Digest.Status)
	assert.Equal(t, "single digest", got.Body)
	assert.Equal(t, len(content), got.Digest.InputBytes)
	assert.Equal(t, len("single digest"), got.Digest.OutputBytes)
	assert.Equal(t, 1, client.count())
	assert.Equal(t, 200, client.calls[0].MaxOutputTokens)
}

func TestFetchWithContext_MapReduceDigestBoundedAndCleansChunks(t *testing.T) {
	content := strings.Repeat("x", 180)
	client := &fakePromptClient{handler: func(call promptCall) (string, error) {
		if strings.Contains(call.User, "Digest one chunk") {
			time.Sleep(5 * time.Millisecond)
			return "chunk summary", nil
		}
		return "reduced digest", nil
	}}
	server := plainTextServer(t, content)

	workspace, err := os.MkdirTemp(t.TempDir(), "workspace-*")
	require.NoError(t, err)
	digester := NewDigester(client, digestTestConfig(), "small-model", func(string) string { return workspace })
	ctx := skill.WithUserID(context.Background(), "user-1")

	res, err := FetchWithContext(ctx, server.URL, "map objective", http.MethodGet, nil, time.Second, 1, Options{MaxOutputBytes: 1024}, digester)
	require.NoError(t, err)
	got := res.(fetchResult)

	require.NotNil(t, got.Digest)
	assert.Equal(t, ProcessingModeMapReduce, got.ProcessingMode)
	assert.Equal(t, digestStatusOK, got.Digest.Status)
	assert.Equal(t, "reduced digest", got.Body)
	assert.Equal(t, 6, got.Digest.ChunksTotal)
	assert.Equal(t, 6, got.Digest.ChunksSucceeded)
	assert.Equal(t, 0, got.Digest.ChunksFailed)
	assert.Equal(t, 2, got.Digest.Concurrency)
	assert.Equal(t, float64(1), got.Digest.Coverage)
	assert.Equal(t, 7, client.count())
	assert.Equal(t, 2, client.maximumActive())
	assert.Contains(t, client.calls[0].System, "untrusted data")

	entries, err := os.ReadDir(filepath.Join(workspace, "tmp"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestFetchWithContext_MapReducePartialFailure(t *testing.T) {
	content := strings.Repeat("y", 216)
	client := &fakePromptClient{handler: func(call promptCall) (string, error) {
		if strings.Contains(call.User, "Digest one chunk") && promptChunkIndex(call.User) == 2 {
			return "", errors.New("chunk model failed")
		}
		if strings.Contains(call.User, "Digest one chunk") {
			return "chunk summary", nil
		}
		return "partial reduced digest", nil
	}}
	server := plainTextServer(t, content)
	digester := NewDigester(client, digestTestConfig(), "small-model", nil)

	res, err := FetchWithContext(context.Background(), server.URL, "", http.MethodGet, nil, time.Second, 1, Options{MaxOutputBytes: 1024}, digester)
	require.NoError(t, err)
	got := res.(fetchResult)

	require.NotNil(t, got.Digest)
	assert.Equal(t, digestStatusPartial, got.Digest.Status)
	assert.Equal(t, got.Digest.ChunksTotal-1, got.Digest.ChunksSucceeded)
	assert.Equal(t, 1, got.Digest.ChunksFailed)
	assert.InDelta(t, 184.0/216.0, got.Digest.Coverage, 0.0001)
	assert.Contains(t, got.Body, "partial reduced digest")
	assert.Contains(t, got.Digest.Error, "chunk 1")
}

func TestFetchWithContext_MapReduceSkipsOversizedFanout(t *testing.T) {
	content := strings.Repeat("x", 40)
	client := &fakePromptClient{handler: func(promptCall) (string, error) {
		return "unexpected", nil
	}}
	server := plainTextServer(t, content)
	cfg := digestTestConfig()
	cfg.ChunkBytes = 8
	cfg.MaxChunks = 2
	digester := NewDigester(client, cfg, "small-model", nil)

	res, err := FetchWithContext(context.Background(), server.URL, "", http.MethodGet, nil, time.Second, 1, Options{MaxOutputBytes: 20}, digester)
	require.NoError(t, err)
	got := res.(fetchResult)

	require.NotNil(t, got.Digest)
	assert.Equal(t, ProcessingModeMapReduce, got.ProcessingMode)
	assert.Equal(t, digestStatusSkipped, got.Digest.Status)
	assert.Contains(t, got.Digest.Error, "max 2")
	assert.Equal(t, 0, client.count())
	assert.True(t, got.Truncated)
	assert.Equal(t, strings.Repeat("x", 14), strings.Split(got.Body, "\n")[0])
}

func TestFetchWithContext_DigestFailureFallsBackToDirectContent(t *testing.T) {
	content := strings.Repeat("f", 24)
	client := &fakePromptClient{handler: func(promptCall) (string, error) {
		return "", errors.New("model unavailable")
	}}
	server := plainTextServer(t, content)
	digester := NewDigester(client, digestTestConfig(), "small-model", nil)

	res, err := FetchWithContext(context.Background(), server.URL, "", http.MethodGet, nil, time.Second, 1, Options{MaxOutputBytes: 1024}, digester)
	require.NoError(t, err)
	got := res.(fetchResult)

	require.NotNil(t, got.Digest)
	assert.Equal(t, digestStatusFailed, got.Digest.Status)
	assert.Contains(t, got.Digest.Error, "model unavailable")
	assert.Equal(t, content, got.Body)
	assert.Equal(t, http.StatusOK, got.StatusCode)
	assert.False(t, got.Truncated)
}

func TestSplitDigestContent_RespectsBoundariesAndUTF8(t *testing.T) {
	content := strings.Repeat("中文", 20)
	chunks := splitDigestContent(content, 12)
	require.NotEmpty(t, chunks)

	var rebuilt strings.Builder
	for _, chunk := range chunks {
		assert.LessOrEqual(t, chunk.End-chunk.Start, 12)
		rebuilt.WriteString(content[chunk.Start:chunk.End])
	}
	assert.Equal(t, content, rebuilt.String())
}

func promptChunkIndex(prompt string) int {
	for _, line := range strings.Split(prompt, "\n") {
		if !strings.HasPrefix(line, "Chunk: ") {
			continue
		}
		value := strings.TrimPrefix(line, "Chunk: ")
		value = strings.Split(value, "/")[0]
		index, _ := strconv.Atoi(value)
		return index
	}
	return 0
}

type fakeContentDigester struct {
	request DigestRequest
}

func (f *fakeContentDigester) ShouldDigest(int) bool { return true }

func (f *fakeContentDigester) Digest(_ context.Context, req DigestRequest) (DigestResult, error) {
	f.request = req
	return DigestResult{
		Mode:        ProcessingModeSingleShot,
		Status:      digestStatusOK,
		Content:     "registered digest",
		InputBytes:  len(req.Content),
		OutputBytes: len("registered digest"),
		Coverage:    1,
	}, nil
}

func TestRegisterAllWithDigester_PassesObjective(t *testing.T) {
	server := plainTextServer(t, "registered page")
	r := tool.NewRegistry()
	digester := &fakeContentDigester{}
	RegisterAllWithDigester(r, config.WebfetchConfig{Enabled: true}, digester)
	spec, ok := r.Get(Name)
	require.True(t, ok)

	args := fmt.Sprintf(`{"url":%q,"objective":"registration objective"}`, server.URL)
	res, err := spec.Execute(context.Background(), []byte(args))
	require.NoError(t, err)
	got := res.(fetchResult)

	assert.Equal(t, "registered digest", got.Body)
	assert.Equal(t, ProcessingModeSingleShot, got.ProcessingMode)
	assert.Equal(t, "registration objective", digester.request.Objective)
	assert.Equal(t, server.URL, digester.request.URL)
}
