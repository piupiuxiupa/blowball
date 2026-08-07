package xizhi

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/bmatcuk/doublestar/v4"
)

// Built-in result caps that bound the response size when an agent searches a
// large workspace (e.g. searching for "the"). maxGrepMatches caps the total
// number of matches across all files; once reached scanning stops and the
// result's truncated flag is set. maxGrepLineRunes caps each returned line
// (matched or context) so a single very long line cannot dominate the output.
const (
	maxGrepMatches   = 200
	maxGrepLineRunes = 500
)

// grepMatch is one content match returned by xizhi_grep.
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

// grepResult is the JSON-serializable result returned by GrepFiles.
type grepResult struct {
	Path       string      `json:"path"`
	Pattern    string      `json:"pattern"`
	Glob       string      `json:"glob,omitempty"`
	IgnoreCase bool        `json:"ignore_case"`
	Matches    []grepMatch `json:"matches"`
	Truncated  bool        `json:"truncated"`
}

// GrepFiles searches the content of files beneath workspaceRoot/relPath for
// lines matching the RE2 pattern. The search path is normalised so an empty
// string or "." refers to the workspace root, and is validated by validatePath
// (absolute paths, "..", symlink escapes and the reserved .blowball namespace
// are rejected). Symlinks are not followed during the walk, mirroring
// xizhi_glob_files.
//
// pattern is a required Go RE2 regular expression; ignoreCase compiles it
// case-insensitively (equivalent to wrapping it in (?i)). glob, when non-empty,
// filters files by their base name via a doublestar match (e.g. "*.go"). Files
// whose leading bytes contain a NUL byte are treated as binary and skipped.
// includeHidden controls whether hidden entries (names beginning with ".") are
// searched. contextBefore / contextAfter add that many surrounding lines to
// each match (omitted from the JSON when zero).
//
// The result is bounded: at most maxGrepMatches matches are returned and each
// line is capped at maxGrepLineRunes runes; when the match cap is hit scanning
// stops and truncated is set to true.
func GrepFiles(workspaceRoot, relPath, pattern, glob string, ignoreCase, includeHidden bool, contextBefore, contextAfter int) (any, error) {
	relPath = normalizePath(relPath)
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}

	if pattern == "" {
		return nil, fmt.Errorf("xizhi_grep: pattern is required")
	}

	expr := pattern
	if ignoreCase {
		expr = "(?i)" + pattern
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("xizhi_grep: invalid regex %q: %w", pattern, err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("directory not found: %w", err)
		}
		return nil, fmt.Errorf("xizhi grep: stat %q: %w", absPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("xizhi grep: %q is not a directory", relPath)
	}

	result := grepResult{
		Path:       relPath,
		Pattern:    pattern,
		Glob:       glob,
		IgnoreCase: ignoreCase,
		Matches:    []grepMatch{},
	}

	walkErr := filepath.WalkDir(absPath, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip unreadable entries rather than aborting the whole search.
			return nil
		}
		if d.IsDir() {
			if p != absPath && !includeHidden && isHiddenName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// Do not follow symlinks (mirrors xizhi_glob_files' WithNoFollow).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !includeHidden && isHiddenName(d.Name()) {
			return nil
		}
		if glob != "" {
			if ok, _ := doublestar.Match(glob, d.Name()); !ok {
				return nil
			}
		}

		remaining := maxGrepMatches - len(result.Matches)
		if remaining <= 0 {
			result.Truncated = true
			return filepath.SkipAll
		}

		fileRel, _ := filepath.Rel(absPath, p)
		matches, hitCap := scanGrepFile(p, filepath.ToSlash(fileRel), re, contextBefore, contextAfter, remaining)
		result.Matches = append(result.Matches, matches...)
		if hitCap {
			result.Truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("xizhi grep: walk %q: %w", absPath, walkErr)
	}

	return result, nil
}

// scanGrepFile reads a single file and returns the matches found within budget.
// A file that cannot be opened or is binary (leading bytes contain a NUL byte,
// aligning with grep -I and workspace WriteContent's binary judgment) yields no
// matches and no error. hitCap reports whether the per-call budget was reached
// so the caller can stop the walk and flag truncation.
func scanGrepFile(absFile, fileRel string, re *regexp.Regexp, contextBefore, contextAfter, budget int) ([]grepMatch, bool) {
	f, err := os.Open(absFile)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	// Binary detection: a NUL byte in the leading bytes marks the file binary.
	if preview, _ := reader.Peek(8192); bytes.IndexByte(preview, 0) >= 0 {
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

	var matches []grepMatch
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		m := grepMatch{
			File:       fileRel,
			LineNumber: i + 1,
			Line:       truncateGrepLine(line),
		}
		if contextBefore > 0 {
			start := i - contextBefore
			if start < 0 {
				start = 0
			}
			m.ContextBefore = truncateGrepLines(lines[start:i])
		}
		if contextAfter > 0 {
			end := i + contextAfter + 1
			if end > len(lines) {
				end = len(lines)
			}
			m.ContextAfter = truncateGrepLines(lines[i+1 : end])
		}
		matches = append(matches, m)
		if len(matches) >= budget {
			return matches, true
		}
	}
	return matches, false
}

// truncateGrepLine caps s at maxGrepLineRunes runes.
func truncateGrepLine(s string) string {
	if len([]rune(s)) <= maxGrepLineRunes {
		return s
	}
	return string([]rune(s)[:maxGrepLineRunes])
}

// truncateGrepLines caps each entry at maxGrepLineRunes runes.
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
