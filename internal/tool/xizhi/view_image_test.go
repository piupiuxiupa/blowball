package xizhi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lush/blowball/internal/config"
	"github.com/lush/blowball/internal/tool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVisionClient records what the tool handed to the model boundary.
type fakeVisionClient struct {
	calls       int
	gotQuestion string
	gotImage    []byte
	gotMIME     string
	description string
	err         error
	blockCtx    bool
}

func (f *fakeVisionClient) Describe(ctx context.Context, question string, image []byte, mime string) (string, error) {
	f.calls++
	f.gotQuestion = question
	f.gotImage = image
	f.gotMIME = mime
	if f.blockCtx {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if f.err != nil {
		return "", f.err
	}
	return f.description, nil
}

// pngHeader is enough for sniffImageMIME (magic-byte detection only).
var pngHeader = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}

func writeTestImage(t *testing.T, content []byte) (root, rel string) {
	t.Helper()
	root = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "shots"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "shots", "img.png"), content, 0o644))
	return root, "shots/img.png"
}

func viewCfg() config.ViewImageConfig {
	return config.ViewImageConfig{
		Enabled:         true,
		Model:           "glm-4v-flash",
		MaxImageBytes:   1 << 20,
		MaxOutputTokens: 2048,
		Timeout:         5 * time.Second,
	}
}

func TestSniffImageMIME(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		mime  string
		valid bool
	}{
		{"png", pngHeader, "image/png", true},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg", true},
		{"gif", append([]byte("GIF89a"), 1, 2), "image/gif", true},
		{"webp", append(append([]byte("RIFF"), 1, 2, 3, 4), []byte("WEBP")...), "image/webp", true},
		{"text", []byte("%PDF-1.7 not an image"), "", false},
		{"empty", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mime, ok := sniffImageMIME(tc.data)
			assert.Equal(t, tc.valid, ok)
			assert.Equal(t, tc.mime, mime)
		})
	}
}

func TestViewImage_SuccessReturnsDescription(t *testing.T) {
	root, rel := writeTestImage(t, pngHeader)
	fake := &fakeVisionClient{description: "a red error dialog"}

	got, err := ViewImage(context.Background(), root, rel, "what is the error?", viewCfg(), "glm-4v-flash", fake)
	require.NoError(t, err)
	assert.Equal(t, rel, got.Path)
	assert.Equal(t, "image/png", got.MIMEType)
	assert.Equal(t, "glm-4v-flash", got.Model)
	assert.Equal(t, "a red error dialog", got.Description)
	assert.EqualValues(t, len(pngHeader), got.Size)

	assert.Equal(t, 1, fake.calls)
	assert.Equal(t, "what is the error?", fake.gotQuestion)
	assert.Equal(t, pngHeader, fake.gotImage)
	assert.Equal(t, "image/png", fake.gotMIME)
}

func TestViewImage_DefaultQuestionWhenOmitted(t *testing.T) {
	root, rel := writeTestImage(t, pngHeader)
	fake := &fakeVisionClient{description: "d"}

	_, err := ViewImage(context.Background(), root, rel, "   ", viewCfg(), "glm-4v-flash", fake)
	require.NoError(t, err)
	assert.Equal(t, defaultViewImageQuestion, fake.gotQuestion)
}

func TestViewImage_RejectsPathOutsideWorkspace(t *testing.T) {
	fake := &fakeVisionClient{}
	_, err := ViewImage(context.Background(), t.TempDir(), "../../etc/passwd", "q", viewCfg(), "m", fake)
	require.Error(t, err)
	assert.True(t, IsPathOutsideWorkspace(err))
	assert.Zero(t, fake.calls, "no model call may happen for a rejected path")
}

func TestViewImage_FileNotFound(t *testing.T) {
	fake := &fakeVisionClient{}
	_, err := ViewImage(context.Background(), t.TempDir(), "missing.png", "q", viewCfg(), "m", fake)
	require.Error(t, err)
	assert.True(t, IsFileNotFound(err))
	assert.Zero(t, fake.calls)
}

func TestViewImage_ExtensionLieRejectedByMagicBytes(t *testing.T) {
	root, rel := writeTestImage(t, []byte("%PDF-1.7 definitely not a png"))
	fake := &fakeVisionClient{}

	_, err := ViewImage(context.Background(), root, rel, "q", viewCfg(), "m", fake)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported image format")
	assert.Zero(t, fake.calls, "unsupported formats must fail before any model call")
}

func TestViewImage_ExceedsMaxImageBytes(t *testing.T) {
	root, rel := writeTestImage(t, pngHeader)
	fake := &fakeVisionClient{}
	cfg := viewCfg()
	cfg.MaxImageBytes = 4

	_, err := ViewImage(context.Background(), root, rel, "q", cfg, "m", fake)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_image_bytes")
	assert.Zero(t, fake.calls)
}

func TestViewImage_ModelErrorSurfacesAsToolError(t *testing.T) {
	root, rel := writeTestImage(t, pngHeader)
	fake := &fakeVisionClient{err: errors.New("gateway 500")}

	_, err := ViewImage(context.Background(), root, rel, "q", viewCfg(), "m", fake)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gateway 500")
}

func TestViewImage_TimeoutBoundsModelCall(t *testing.T) {
	root, rel := writeTestImage(t, pngHeader)
	fake := &fakeVisionClient{blockCtx: true}
	cfg := viewCfg()
	cfg.Timeout = 20 * time.Millisecond

	_, err := ViewImage(context.Background(), root, rel, "q", cfg, "m", fake)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestRegisterAll_ViewImageGating(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		r := tool.NewRegistry()
		RegisterAll(r, t.TempDir(), config.XizhiConfig{}, &fakeVisionClient{})
		_, ok := r.Get(NameViewImage)
		assert.False(t, ok)
	})

	t.Run("enabled registers the tool", func(t *testing.T) {
		r := tool.NewRegistry()
		cfg := config.XizhiConfig{ViewImage: viewCfg()}
		RegisterAll(r, t.TempDir(), cfg, &fakeVisionClient{description: "d"})
		spec, ok := r.Get(NameViewImage)
		require.True(t, ok)
		assert.Equal(t, NameViewImage, spec.Name)
		assert.Contains(t, spec.Description, "question")
	})

	t.Run("enabled without client panics at wiring time", func(t *testing.T) {
		assert.Panics(t, func() {
			RegisterAll(tool.NewRegistry(), t.TempDir(), config.XizhiConfig{ViewImage: viewCfg()}, nil)
		})
	})
}

func TestRegisterAll_ViewImageExecutableViaRegistry(t *testing.T) {
	root, rel := writeTestImage(t, pngHeader)
	fake := &fakeVisionClient{description: "registry-driven description"}
	r := tool.NewRegistry()
	RegisterAll(r, root, config.XizhiConfig{ViewImage: viewCfg()}, fake)

	out, err := r.Call(context.Background(), NameViewImage, []byte(`{"path":`+mustJSONString(rel)+`,"question":"what is this?"}`))
	require.NoError(t, err)
	res, ok := out.(viewImageResult)
	require.True(t, ok, "registry call must return the logical result object")
	assert.Equal(t, "registry-driven description", res.Description)
	assert.Equal(t, "image/png", res.MIMEType)
	assert.Equal(t, "what is this?", fake.gotQuestion)
}

func mustJSONString(s string) string {
	return `"` + s + `"`
}
