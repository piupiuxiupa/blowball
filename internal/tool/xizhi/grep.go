package xizhi

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Built-in result caps that bound the response size when an agent searches a
// large workspace (e.g. searching for "the"). maxGrepMatches is the default
// head_limit (the historical hard cap, preserved so default behavior is
// unchanged); maxGrepLineRunes caps each returned line (matched or context) so a
// single very long line cannot dominate the output. See grep_engine.go for the
// pagination layer (head_limit/offset) and the maxGrepCollect memory ceiling.
const (
	maxGrepMatches   = 200
	maxGrepLineRunes = 500
	// grepReadBufferSize is the buffered reader size used while scanning text.
	grepReadBufferSize = 64 * 1024
	// maxGrepLineBytes preserves the prior bufio.Scanner limit: files with a
	// line longer than 1 MiB are silently skipped as non-line-oriented text.
	maxGrepLineBytes = 1 << 20
)

// errGrepLineTooLong is internal to readTextLines; it makes a bounded reader
// distinguish an oversized line (silently skipped) from cancellation (an
// error).
var errGrepLineTooLong = errors.New("grep line too long")

// grepMatch is one content match returned by xizhi_grep in content mode.
type grepMatch struct {
	// File is the path of the matched file relative to the search path.
	File string `json:"file"`
	// LineNumber is the 1-based number of the matched line.
	LineNumber int `json:"line_number"`
	// Line is the matched line's text (truncated to maxGrepLineRunes runes).
	Line string `json:"line"`
	// ContextBefore / ContextAfter carry the requested context lines (relative
	// to the matched line) and are omitted when the corresponding parameter is
	// zero. They are capped to maxGrepLineRunes runes per line.
	ContextBefore []string `json:"context_before,omitempty"`
	ContextAfter  []string `json:"context_after,omitempty"`
}

// grepCount is one per-file match tally returned in count mode.
type grepCount struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}

// grepResult is the JSON-serializable result returned by xizhi_grep. The
// populated collection depends on Mode: Matches (content), Files
// (files_with_matches) or Counts (count). Matches is always present (possibly
// empty) for backward compatibility; Files/Counts are populated only in their
// mode. Truncated, AppliedLimit, AppliedOffset and TotalFiles describe the
// pagination window so the model can request the next page.
type grepResult struct {
	Path          string      `json:"path"`
	Pattern       string      `json:"pattern"`
	Glob          string      `json:"glob,omitempty"`
	IgnoreCase    bool        `json:"ignore_case"`
	Mode          string      `json:"mode"`
	Matches       []grepMatch `json:"matches"`
	Files         []string    `json:"files,omitempty"`
	Counts        []grepCount `json:"counts,omitempty"`
	Truncated     bool        `json:"truncated"`
	AppliedLimit  int         `json:"applied_limit"`
	AppliedOffset int         `json:"applied_offset"`
	TotalFiles    int         `json:"total_files"`
}

// goGrepEngine is the pure-Go RE2 fallback used when ripgrep is not installed.
// It walks the search path and matches each text file with the compiled regex.
// It produces the same rawMatch shape as the ripgrep engine; the shared mapper
// in grep_engine.go normalizes ordering, pagination and truncation so both
// engines yield an identical grepResult.
type goGrepEngine struct{}

func (goGrepEngine) search(ctx context.Context, in grepInput) (engineResult, error) {
	var er engineResult
	walkErr := filepath.WalkDir(in.absPath, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// Skip unreadable entries rather than aborting the whole search.
			return nil
		}
		if d.IsDir() {
			if isWorkspaceReservedNamespace(in, p) {
				return filepath.SkipDir
			}
			if p != in.absPath && !in.includeHidden && isHiddenName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// Do not follow symlinks (skipped entirely, like the find engines).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// Hidden filtering applies to entries discovered by the walk, never to
		// an explicitly targeted single-file root (rg searches explicit hidden
		// path arguments too). In directory mode the root is a directory, so
		// the guard never fires there.
		if p != in.absPath && !in.includeHidden && isHiddenName(d.Name()) {
			return nil
		}
		if in.glob != "" {
			if ok, _ := doublestar.Match(in.glob, d.Name()); !ok {
				return nil
			}
		}

		fileRel, _ := filepath.Rel(in.absPath, p)
		if in.searchFile != "" {
			// Single-file mode: the walk visits exactly the target file; its
			// match file is the basename (Rel of a path against itself is ".").
			fileRel = in.searchFile
		}
		matches, err := scanGrepFileRaw(ctx, p, filepath.ToSlash(fileRel), in.re, in.contextBefore, in.contextAfter)
		if err != nil {
			return err
		}
		er.matches = append(er.matches, matches...)
		if len(er.matches) >= maxGrepCollect {
			er.collectCapped = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		if ctx.Err() != nil {
			return engineResult{}, ctx.Err()
		}
		return engineResult{}, fmt.Errorf("xizhi grep: walk %q: %w", in.absPath, walkErr)
	}
	return er, nil
}

// scanGrepFileRaw reads a single file and returns every match as a rawMatch
// (untruncated line + raw context lines). A file that cannot be opened or is
// binary (leading bytes contain a NUL byte, aligning with grep -I) yields no
// matches and no error. Truncation of long lines is applied later by the shared
// mapper so this stays engine-agnostic.
func scanGrepFileRaw(ctx context.Context, absFile, fileRel string, re *regexp.Regexp, contextBefore, contextAfter int) ([]rawMatch, error) {
	lines, ok, err := readTextLines(ctx, absFile)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	var out []rawMatch
	for i, line := range lines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !re.MatchString(line) {
			continue
		}
		m := rawMatch{file: fileRel, lineNumber: i + 1, line: line}
		m.contextBefore, m.contextAfter = contextAround(lines, i+1, contextBefore, contextAfter)
		out = append(out, m)
	}
	return out, nil
}

// readTextLines reads absFile and returns its lines. It reports ok=false (and a
// nil error) if the file cannot be opened, is binary, or has a line that
// exceeds the reader limit — matching the engines' silent-skip behavior. CRLF
// line endings are normalized. A cancelled context is returned as err so a
// deadline is never mistaken for an empty result.
func readTextLines(ctx context.Context, absFile string) ([]string, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	f, err := os.Open(absFile)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, nil
	}
	defer f.Close()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}

	reader := bufio.NewReaderSize(f, grepReadBufferSize)
	// Binary detection: a NUL byte in the leading bytes marks the file binary.
	preview, _ := reader.Peek(binarySniffPeek)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if looksBinary(preview) {
		return nil, false, nil
	}

	var lines []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		line, err := readTextLine(ctx, reader)
		if errors.Is(err, io.EOF) {
			return lines, true, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			if errors.Is(err, errGrepLineTooLong) {
				return nil, false, nil
			}
			// Other read errors retain the historical silent-skip behavior.
			return nil, false, nil
		}
		lines = append(lines, line)
	}
}

// readTextLine reads one newline-terminated line without loading an unbounded
// token into memory. It returns io.EOF only when the file ends with no pending
// bytes.
func readTextLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	var line []byte
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fragment, err := reader.ReadSlice('\n')
		newline := err == nil
		length := len(line) + len(fragment)
		if newline {
			length--
		}
		if length > maxGrepLineBytes {
			if err := drainTextLine(ctx, reader); err != nil && ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errGrepLineTooLong
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) == 0 {
					return "", io.EOF
				}
				// Return the final unterminated line successfully; the next
				// read will report EOF with no pending bytes.
				return trimGrepEOL(string(line)), nil
			}
			return "", err
		}
		return trimGrepEOL(string(line)), nil
	}
}

// drainTextLine consumes the remainder of an oversized line so the reader is
// positioned at the next line before reporting the skip. Cancellation still
// takes priority over the silent-skip classification.
func drainTextLine(ctx context.Context, reader *bufio.Reader) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := reader.Discard(grepReadBufferSize)
		if errors.Is(err, io.EOF) || (err != nil && !errors.Is(err, bufio.ErrBufferFull)) {
			return nil
		}
	}
}

// trimGrepEOL removes LF and, like bufio.Scanner's ScanLines, an immediately
// preceding CR.
func trimGrepEOL(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
}

// isWorkspaceReservedNamespace reports whether p is the workspace-level
// reserved namespace encountered during a recursive search rooted at the
// workspace itself. Nested directories with the same name keep ordinary hidden
// entry semantics.
func isWorkspaceReservedNamespace(in grepInput, p string) bool {
	if in.workspaceRoot == "" || in.searchFile != "" {
		return false
	}
	root, err := filepath.Abs(in.workspaceRoot)
	if err != nil || filepath.Clean(root) != filepath.Clean(in.absPath) {
		return false
	}
	rel, err := filepath.Rel(in.absPath, p)
	if err != nil {
		return false
	}
	return firstSegment(rel) == reservedNamespaceDir
}

// contextAround returns the before/after context lines for the 1-based
// lineNumber, given the file's full line slice. It is the single source of
// truth for context slicing so the ripgrep and Go engines produce identical
// context. Nil slices are returned when the corresponding amount is zero (so
// they omit from the JSON via omitempty).
func contextAround(lines []string, lineNumber, before, after int) (contextBefore, contextAfter []string) {
	i := lineNumber - 1
	if before > 0 {
		start := max(0, i-before)
		contextBefore = append([]string(nil), lines[start:i]...)
	}
	if after > 0 {
		end := min(i+after+1, len(lines))
		contextAfter = append([]string(nil), lines[i+1:end]...)
	}
	return contextBefore, contextAfter
}

// truncateGrepLine caps s at maxGrepLineRunes runes.
func truncateGrepLine(s string) string {
	if len([]rune(s)) <= maxGrepLineRunes {
		return s
	}
	return string([]rune(s)[:maxGrepLineRunes])
}

// truncateGrepLines caps each entry at maxGrepLineRunes runes. A nil/empty input
// returns nil so the field omits from the JSON.
func truncateGrepLines(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = truncateGrepLine(s)
	}
	return out
}
