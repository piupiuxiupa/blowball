package xizhi

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

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
)

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
			if p != in.absPath && !in.includeHidden && isHiddenName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// Do not follow symlinks (mirrors xizhi_glob_files' WithNoFollow).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !in.includeHidden && isHiddenName(d.Name()) {
			return nil
		}
		if in.glob != "" {
			if ok, _ := doublestar.Match(in.glob, d.Name()); !ok {
				return nil
			}
		}

		fileRel, _ := filepath.Rel(in.absPath, p)
		er.matches = append(er.matches, scanGrepFileRaw(p, filepath.ToSlash(fileRel), in.re, in.contextBefore, in.contextAfter)...)
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
func scanGrepFileRaw(absFile, fileRel string, re *regexp.Regexp, contextBefore, contextAfter int) []rawMatch {
	lines, ok := readTextLines(absFile)
	if !ok {
		return nil
	}
	var out []rawMatch
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		m := rawMatch{file: fileRel, lineNumber: i + 1, line: line}
		m.contextBefore, m.contextAfter = contextAround(lines, i+1, contextBefore, contextAfter)
		out = append(out, m)
	}
	return out
}

// readTextLines reads absFile and returns its lines. It reports ok=false (no
// error) if the file cannot be opened, is binary, or has a line that exceeds the
// scanner buffer — matching the grep engines' silent-skip behavior. CRLF line
// endings are normalized (the trailing CR is stripped, like bufio.Scanner).
func readTextLines(absFile string) ([]string, bool) {
	f, err := os.Open(absFile)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	// Binary detection: a NUL byte in the leading bytes marks the file binary.
	if preview, _ := reader.Peek(binarySniffPeek); looksBinary(preview) {
		return nil, false
	}

	scanner := bufio.NewScanner(reader)
	// Allow long lines (up to 1 MiB) before Scanner errors out.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	// A scan error (e.g. an oversized line) means the file is not cleanly
	// line-oriented text; skip it rather than returning a partial result.
	if err := scanner.Err(); err != nil {
		return nil, false
	}
	return lines, true
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
