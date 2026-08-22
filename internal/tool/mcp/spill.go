package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lush/blowball/internal/tokens"
	"github.com/lush/blowball/internal/tool/xizhi"
)

// Spill layout constants (mcp-call-result-spill). The spill directory lives
// INSIDE the user's workspace on purpose: the xizhi_* read tools are scoped to
// the workspace root, so a spill file outside it (or under the reserved
// .blowball namespace, which xizhi rejects) would be unreadable by the model
// and the whole read-back loop would be moot.
const (
	// spillDirRel is the workspace-relative directory holding spilled
	// mcp_call results, one subdirectory per server.
	spillDirRel = "tmp/mcp-outputs"
	// previewBytes is the head of the payload kept in the spill/truncation
	// envelope so the model has something actionable without re-reading.
	previewBytes = 1024
)

// spillHint is the read-back guidance carried on every spill envelope. It
// names the two existing tools that already paginate and regex-search
// workspace files — the read side of the feature is entirely preexisting.
const spillHint = "Result too large for inline context; the full output was saved to `path`. " +
	"Read it back selectively with xizhi_read_file (paginated offset/limit) or xizhi_grep (regex)."

// spillEnvelope is the small result returned to the model when an mcp_call
// result is spilled to a workspace file (over the token threshold, or forced
// by the model's output_path parameter). It rides inside the ordinary
// tool-result envelope as the `result` field — {"status":0,"result":{…}}.
type spillEnvelope struct {
	Spilled   bool   `json:"spilled"`
	Path      string `json:"path"`
	TokensEst int    `json:"tokens_est"`
	Size      int    `json:"size"`
	Items     int    `json:"items"`
	Preview   string `json:"preview"`
	Hint      string `json:"hint"`
}

// truncatedEnvelope is the degraded return when a spill WRITE fails (disk
// full, read-only fs, …): the remote call itself succeeded, so the status
// stays 0, but the full payload is never handed back inline — that would
// defeat the very cap this feature exists to enforce.
type truncatedEnvelope struct {
	Truncated bool   `json:"truncated"`
	Size      int    `json:"size"`
	Preview   string `json:"preview"`
	Note      string `json:"note"`
}

// maybeSpill returns what mcp_call hands back to the model for a SUCCESSFUL
// remote result: the untouched callResult when it fits inline, a spillEnvelope
// when it must be written to a file (estimate over the manager's cap, or
// forced by a non-empty outputPath), or a truncatedEnvelope when that write
// fails. A disabled cap (0) skips the estimate entirely unless outputPath
// forces a spill.
func (m *Manager) maybeSpill(serverName, toolName, outputPath, traceID string, res callResult) any {
	forced := strings.TrimSpace(outputPath) != ""
	if m.maxInlineResultTokens <= 0 && !forced {
		return res
	}

	// Estimate on the exact JSON the model would see inline (the design's
	// "model-visible payload"), not on the raw text lengths.
	raw, err := json.Marshal(res)
	if err != nil {
		// Unmarshalable result cannot be estimated; the envelope renderer
		// would fail on it anyway. Keep prior behavior: pass it through.
		return res
	}
	est := tokens.Estimate(string(raw))
	if !forced && est <= m.maxInlineResultTokens {
		return res
	}

	data, ext := spillFilePayload(res)
	var relPath string
	if forced {
		relPath = strings.TrimSpace(outputPath)
	} else {
		relPath = autoSpillRelPath(serverName, toolName, traceID, m.nextSpillSeq(), ext)
	}
	// Auto paths are unique per construction (trace + seq) and must never
	// overwrite; a model-supplied output_path is create-or-overwrite by
	// design (aligned with xizhi_write_file's write mode).
	if err := writeSpillFile(m.workspaceRoot, relPath, data, !forced); err != nil {
		return truncatedEnvelope{
			Truncated: true,
			Size:      len(data),
			Preview:   previewOf(data),
			Note: fmt.Sprintf("full output (%d bytes) could not be saved to file %q: %v",
				len(data), relPath, err),
		}
	}
	return spillEnvelope{
		Spilled:   true,
		Path:      relPath,
		TokensEst: est,
		Size:      len(data),
		Items:     len(res.Content),
		Preview:   previewOf(data),
		Hint:      spillHint,
	}
}

// validateOutputPath confines a model-supplied spill path with the SAME rules
// as the xizhi_* tools: absolute paths, .. traversal, symlink escapes, and the
// reserved .blowball namespace are rejected; the cleaned path must stay inside
// the workspace. mcp_call runs this BEFORE the remote call so a bad path has
// no remote side effects (mirroring the pre-call schema validation).
func validateOutputPath(workspaceRoot, outputPath string) error {
	p := strings.TrimSpace(outputPath)
	if p == "" {
		return nil
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("mcp_call: output_path must be a workspace-relative path, got absolute %q", p)
	}
	if _, err := xizhi.ValidatePath(workspaceRoot, p); err != nil {
		return fmt.Errorf("mcp_call: output_path invalid: %w", err)
	}
	return nil
}

// spillFilePayload renders the spill file body. A single text content item is
// written verbatim (best for xizhi_grep / xizhi_read_file paging); anything
// else — multiple items, or non-text items — is written as a
// structure-preserving JSON array.
func spillFilePayload(res callResult) ([]byte, string) {
	if len(res.Content) == 1 && res.Content[0].Type == "text" {
		return []byte(res.Content[0].Text), ".txt"
	}
	b, err := json.MarshalIndent(res.Content, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf("%v", res.Content))
	}
	return b, ".json"
}

// autoSpillRelPath builds the workspace-relative path for an automatic spill:
// tmp/mcp-outputs/{server}/{tool}-{trace_id}-{seq}{ext}. Every segment is
// filename-sanitized (remote tool names are server-controlled); the trace id
// (unique per turn) plus the manager-scoped sequence number keep names unique
// across turns and across parallel dispatches within a round, so a path quoted
// in earlier history is never silently overwritten by a later spill.
func autoSpillRelPath(serverName, toolName, traceID string, seq int64, ext string) string {
	if strings.TrimSpace(traceID) == "" {
		// Defensive fallback: the agent-path ctx should always carry the
		// turn's trace id; a nanosecond timestamp preserves uniqueness.
		traceID = fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	name := fmt.Sprintf("%s-%s-%d%s",
		sanitizeSegment(toolName), sanitizeSegment(traceID), seq, ext)
	return filepath.ToSlash(filepath.Join(spillDirRel, sanitizeSegment(serverName), name))
}

// sanitizeSegment keeps only filename-safe runes; anything else (including
// path separators) collapses to '_'. An all-unsafe segment maps to "x".
func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "x"
	}
	return out
}

// writeSpillFile writes data to the workspace-relative relPath, creating
// parent directories. exclusive=true (automatic spills) fails if the file
// already exists instead of overwriting; exclusive=false (model-supplied
// output_path) truncates an existing file (create-or-overwrite).
func writeSpillFile(workspaceRoot, relPath string, data []byte, exclusive bool) error {
	abs, err := xizhi.ValidatePath(workspaceRoot, relPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("create spill dir: %w", err)
	}
	flags := os.O_CREATE | os.O_WRONLY
	if exclusive {
		flags |= os.O_EXCL
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flags, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// previewOf returns the head of data (at most previewBytes, never splitting a
// multi-byte rune) with an explicit truncation marker when bytes were cut.
func previewOf(data []byte) string {
	if len(data) <= previewBytes {
		return string(data)
	}
	cut := previewBytes
	for cut > 0 && !utf8.RuneStart(data[cut]) {
		cut--
	}
	return string(data[:cut]) + "…[truncated]"
}
