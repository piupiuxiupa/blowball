package xizhi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lush/blowball/internal/config"
)

// VisionClient is the narrow model boundary used by xizhi_view_image. The
// agent package provides the production implementation (a shared-client
// adapter that sends the image base64-inline to the configured vision model);
// tests use fakes. effort is one of the closed reasoning-effort set
// ("" normalizes to none) chosen by the CALLING agent per invocation.
type VisionClient interface {
	Describe(ctx context.Context, question, effort string, image []byte, mime string) (string, error)
}

// defaultViewImageQuestion is used when the model omits question. A targeted
// question produces markedly better vision output than an open-ended "what is
// this", so the tool description steers the model toward passing one; this
// fallback keeps bare calls useful.
const defaultViewImageQuestion = "Describe this image in detail, including any text visible in it."

// visionEfforts is the closed set accepted by the reasoning_effort tool
// parameter — identical to the chat request axis (model-effort-v2) so agents
// see one vocabulary across the product.
var visionEfforts = map[string]bool{
	"none": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// viewImageResult is the logical result of xizhi_view_image. The description
// is the vision model's TEXT account of the image — the image itself never
// returns to the main model (Chat Completions tool messages are text-only).
type viewImageResult struct {
	Path        string `json:"path"`
	MIMEType    string `json:"mime_type"`
	Size        int64  `json:"size"`
	Model       string `json:"model"`
	Description string `json:"description"`
}

// sniffImageMIME reports the image MIME type implied by the file's leading
// magic bytes (never the extension — an extension can lie). Only the four
// formats the sidecar accepts are recognized.
func sniffImageMIME(data []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "image/png", true
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg", true
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif", true
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp", true
	default:
		return "", false
	}
}

// ViewImage reads the image at relPath inside workspaceRoot and returns the
// designated vision model's text description of it. cfg is the resolved
// view-image configuration and modelName the configured vision model's name
// (reported in the result for traceability). Path validation reuses the xizhi
// sandbox; the MIME type is sniffed from magic bytes; oversized or
// non-image files are rejected before any model call is made. The cfg timeout
// bounds the whole operation (the registry-level tools.timeouts entry, when
// present, acts as a coarser outer backstop).
func ViewImage(ctx context.Context, workspaceRoot, relPath, question, effort string, cfg config.ViewImageConfig, modelName string, client VisionClient) (viewImageResult, error) {
	if client == nil {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: vision client is nil")
	}
	if effort == "" {
		effort = "none"
	}
	if !visionEfforts[effort] {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: invalid reasoning_effort %q (must be none, low, medium, high, xhigh or max)", effort)
	}

	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return viewImageResult{}, fmt.Errorf("xizhi_view_image: %w", ErrFileNotFound)
		}
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: stat: %w", err)
	}
	if info.IsDir() {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: %w: %q is a directory", ErrFileNotFound, relPath)
	}
	if info.Size() > int64(cfg.MaxImageBytes) {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: image exceeds max_image_bytes (%d > %d)", info.Size(), cfg.MaxImageBytes)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: read: %w", err)
	}
	mime, ok := sniffImageMIME(data)
	if !ok {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: unsupported image format (supported: image/png, image/jpeg, image/webp, image/gif)")
	}

	q := strings.TrimSpace(question)
	if q == "" {
		q = defaultViewImageQuestion
	}

	var cancel context.CancelFunc
	if cfg.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	description, err := client.Describe(ctx, q, effort, data, mime)
	if err != nil {
		return viewImageResult{}, fmt.Errorf("xizhi_view_image: %w", err)
	}
	return viewImageResult{
		Path:        relPath,
		MIMEType:    mime,
		Size:        info.Size(),
		Model:       modelName,
		Description: description,
	}, nil
}
