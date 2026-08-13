package xizhi

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// defaultReadLimit is the maximum number of lines ReadFile returns by default,
// mirroring Claude Code's Read tool. Callers paginate larger files with the
// offset and limit parameters.
const defaultReadLimit = 2000

// readResult is the JSON-serializable result returned by ReadFile. Content is
// rendered in cat -n form ("<lineNo>\t<line>", one line per row) so the model
// can cite results as file:line. StartLine, TotalLines and Truncated let it
// paginate files larger than the returned window.
type readResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"start_line"`
	TotalLines int    `json:"total_lines"`
	Size       int    `json:"size"`
	Truncated  bool   `json:"truncated"`
}

// ReadFile reads the file at relPath inside workspaceRoot and returns its
// content as cat -n line-numbered text. A window of up to limit lines starting
// at the 1-based offset is returned; when the file has lines beyond the window,
// truncated is set and total_lines reports the full line count so the caller can
// request the next page.
//
// A missing file is reported as ErrFileNotFound. A file whose leading bytes
// contain a NUL byte is treated as binary and rejected with an error rather
// than returned as garbled content. offset<=0 means line 1; limit<=0 means
// defaultReadLimit.
func ReadFile(workspaceRoot, relPath string, offset, limit int) (any, error) {
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q", ErrFileNotFound, relPath)
		}
		return nil, fmt.Errorf("xizhi read: %q: %w", absPath, err)
	}
	if looksBinary(contents) {
		return nil, fmt.Errorf("xizhi_read_file: %q appears to be binary; binary files are not readable as text", relPath)
	}

	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}

	lines := splitLines(contents)
	totalLines := len(lines)

	start := min(offset-1, totalLines)
	end := min(start+limit, totalLines)

	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, lines[i])
	}

	return readResult{
		Path:       relPath,
		Content:    strings.TrimSuffix(b.String(), "\n"),
		StartLine:  start + 1,
		TotalLines: totalLines,
		Size:       len(contents),
		Truncated:  end < totalLines,
	}, nil
}

// splitLines splits data into lines, normalizing CRLF to LF and dropping the
// single trailing empty element produced by a final newline (so "a\nb\n" yields
// ["a","b"], matching cat -n and line-number expectations). An empty file yields
// no lines.
func splitLines(data []byte) []string {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		// A trailing newline creates a final empty element that is not a real
		// line; drop it. ("a\nb" without a trailing newline is left intact.)
		lines = lines[:len(lines)-1]
	}
	return lines
}
