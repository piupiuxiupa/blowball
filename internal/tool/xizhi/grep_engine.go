package xizhi

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// xizhi_grep output modes. The default (and any unrecognized value) is content,
// preserving the tool's historical behavior.
const (
	outputModeContent = "content"
	outputModeFiles   = "files_with_matches"
	outputModeCount   = "count"
)

// defaultGrepHeadLimit bounds the number of entries returned when the caller
// does not request pagination. It equals the historical hard cap so default
// behavior is unchanged.
const defaultGrepHeadLimit = maxGrepMatches

// maxGrepCollect is a memory-safety ceiling on how many matches an engine
// collects before stopping. Hitting it sets the result's truncated flag and
// makes the file/match totals a lower bound. It sits well above the default head
// limit so pagination still works for typical deep searches while bounding
// memory for pathological patterns (e.g. matching every line).
const maxGrepCollect = 10000

// grepInput is the validated input shared by every grep engine.
type grepInput struct {
	relPath       string // workspace-relative search path (for the result Path field)
	absPath       string // resolved absolute search root
	pattern       string
	glob          string
	ignoreCase    bool
	includeHidden bool
	contextBefore int
	contextAfter  int
	outputMode    string
	headLimit     int
	offset        int
	re            *regexp.Regexp // compiled pattern (Go engine); rg recompiles its own
}

// rawMatch is a single match collected by an engine before the shared mapper
// applies ordering, pagination and line truncation. Context lines are populated
// only in content mode (when contextBefore/contextAfter > 0).
type rawMatch struct {
	file          string
	lineNumber    int
	line          string
	contextBefore []string
	contextAfter  []string
}

// engineResult is what an engine returns: the full (bounded) match list plus a
// flag indicating the internal collect ceiling was hit, which makes the totals a
// lower bound.
type engineResult struct {
	matches       []rawMatch
	collectCapped bool
}

// grepEngine abstracts the search backend so xizhi_grep can use ripgrep when it
// is installed and fall back to a pure-Go RE2 walker otherwise. Both engines
// return matches in the same rawMatch shape; buildGrepResult normalizes order,
// pagination and truncation so the final grepResult is identical regardless of
// engine.
type grepEngine interface {
	search(ctx context.Context, in grepInput) (engineResult, error)
}

// defaultEngine resolves the grep engine once per process: ripgrep if present on
// PATH, otherwise the Go fallback. The lookup is cached so repeated turns do not
// re-probe.
var defaultEngine = sync.OnceValue(func() grepEngine {
	if p, err := exec.LookPath("rg"); err == nil {
		return rgGrepEngine{rgPath: p}
	}
	return goGrepEngine{}
})

// GrepSearch is the full-parameter entry point used by the tool registry: it
// validates inputs, delegates to the resolved grep engine (ripgrep when
// available, Go fallback otherwise), and maps the raw matches into the final
// paginated result.
func GrepSearch(
	workspaceRoot, relPath, pattern, glob string,
	ignoreCase, includeHidden bool,
	contextBefore, contextAfter int,
	outputMode string, headLimit, offset int,
) (any, error) {
	return grepRun(context.Background(), workspaceRoot, relPath, pattern, glob, ignoreCase, includeHidden, contextBefore, contextAfter, outputMode, headLimit, offset)
}

// GrepFiles is the legacy entry retained for backward compatibility (existing
// callers/tests). It searches in content mode with the default head limit and no
// offset.
func GrepFiles(workspaceRoot, relPath, pattern, glob string, ignoreCase, includeHidden bool, contextBefore, contextAfter int) (any, error) {
	return grepRun(context.Background(), workspaceRoot, relPath, pattern, glob, ignoreCase, includeHidden, contextBefore, contextAfter, outputModeContent, defaultGrepHeadLimit, 0)
}

func grepRun(
	ctx context.Context,
	workspaceRoot, relPath, pattern, glob string,
	ignoreCase, includeHidden bool,
	contextBefore, contextAfter int,
	outputMode string, headLimit, offset int,
) (any, error) {
	if strings.TrimSpace(relPath) == "" {
		return nil, fmt.Errorf("xizhi_grep: path is required")
	}
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

	// Normalize output mode: empty -> content. The schema enumerates the valid
	// values, so any other string is a model error treated as content.
	switch outputMode {
	case outputModeContent, outputModeFiles, outputModeCount:
	default:
		outputMode = outputModeContent
	}
	if headLimit <= 0 {
		headLimit = defaultGrepHeadLimit
	}
	if offset < 0 {
		offset = 0
	}
	// Context only applies to content mode; drop it otherwise so non-content
	// engines do no wasted context work.
	if outputMode != outputModeContent {
		contextBefore, contextAfter = 0, 0
	}

	in := grepInput{
		relPath:       relPath,
		absPath:       absPath,
		pattern:       pattern,
		glob:          glob,
		ignoreCase:    ignoreCase,
		includeHidden: includeHidden,
		contextBefore: contextBefore,
		contextAfter:  contextAfter,
		outputMode:    outputMode,
		headLimit:     headLimit,
		offset:        offset,
		re:            re,
	}

	er, err := defaultEngine().search(ctx, in)
	if err != nil {
		return nil, err
	}
	return buildGrepResult(in, er), nil
}

// buildGrepResult maps an engine's raw matches into the final, paginated
// grepResult. It sorts matches deterministically (file, then line number) so the
// output is identical regardless of which engine produced it or in what order,
// then applies offset/head_limit pagination per the mode's unit (matches for
// content, file paths for files_with_matches, count entries for count).
func buildGrepResult(in grepInput, er engineResult) grepResult {
	res := grepResult{
		Path:       in.relPath,
		Pattern:    in.pattern,
		Glob:       in.glob,
		IgnoreCase: in.ignoreCase,
		Mode:       in.outputMode,
		Matches:    []grepMatch{},
	}

	sort.SliceStable(er.matches, func(i, j int) bool {
		if er.matches[i].file != er.matches[j].file {
			return er.matches[i].file < er.matches[j].file
		}
		return er.matches[i].lineNumber < er.matches[j].lineNumber
	})

	switch in.outputMode {
	case outputModeFiles:
		files := distinctFiles(er.matches)
		total := len(files)
		start, end := pageWindow(total, in.offset, in.headLimit)
		res.Files = files[start:end]
		res.TotalFiles = total
		res.Truncated = in.offset+in.headLimit < total || er.collectCapped
	case outputModeCount:
		counts := countByFile(er.matches)
		total := len(counts)
		start, end := pageWindow(total, in.offset, in.headLimit)
		res.Counts = counts[start:end]
		res.TotalFiles = total
		res.Truncated = in.offset+in.headLimit < total || er.collectCapped
	default: // content
		total := len(er.matches)
		start, end := pageWindow(total, in.offset, in.headLimit)
		for _, m := range er.matches[start:end] {
			res.Matches = append(res.Matches, grepMatch{
				File:          m.file,
				LineNumber:    m.lineNumber,
				Line:          truncateGrepLine(m.line),
				ContextBefore: truncateGrepLines(m.contextBefore),
				ContextAfter:  truncateGrepLines(m.contextAfter),
			})
		}
		res.TotalFiles = len(distinctFiles(er.matches))
		res.Truncated = in.offset+in.headLimit < total || er.collectCapped
	}

	res.AppliedLimit = in.headLimit
	res.AppliedOffset = in.offset
	return res
}

// pageWindow clamps [offset, offset+limit) to [0, total] and returns the bounds.
func pageWindow(total, offset, limit int) (int, int) {
	offset = min(offset, total)
	return offset, min(offset+limit, total)
}

// distinctFiles returns the files of the matches in order of first appearance
// (after sorting, that is lexical order), without duplicates.
func distinctFiles(ms []rawMatch) []string {
	seen := make(map[string]struct{}, len(ms))
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if _, ok := seen[m.file]; ok {
			continue
		}
		seen[m.file] = struct{}{}
		out = append(out, m.file)
	}
	return out
}

// countByFile returns per-file match tallies in order of first appearance.
func countByFile(ms []rawMatch) []grepCount {
	counts := make(map[string]int, len(ms))
	order := make([]string, 0, len(ms))
	for _, m := range ms {
		if _, ok := counts[m.file]; !ok {
			order = append(order, m.file)
		}
		counts[m.file]++
	}
	out := make([]grepCount, 0, len(order))
	for _, f := range order {
		out = append(out, grepCount{File: f, Count: counts[f]})
	}
	return out
}
