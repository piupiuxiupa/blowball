package xizhi

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Entry-type filter values accepted by xizhi_find's `type` parameter.
const (
	findTypeFile      = "file"
	findTypeDirectory = "directory"
	findTypeAny       = "any"
)

// defaultFindHeadLimit bounds the number of entries returned when the caller
// does not request pagination (the same default xizhi_grep uses).
const defaultFindHeadLimit = 200

// maxFindCollect is a memory-safety ceiling on how many entries an engine
// collects before stopping, mirroring maxGrepCollect. Hitting it sets the
// result's truncated flag and makes total a lower bound.
const maxFindCollect = 10000

// findEntry is one matched workspace entry. Path is relative to the search
// root (slash-separated); Type is "file" or "directory".
type findEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// findResult is the JSON-serializable result returned by xizhi_find. Entries
// is always present (possibly empty). Truncated, AppliedLimit and
// AppliedOffset describe the pagination window so the model can request the
// next page; Total is the number of matched entries before pagination (a
// lower bound when the collect ceiling was hit).
type findResult struct {
	Path          string      `json:"path"`
	Pattern       string      `json:"pattern"`
	Type          string      `json:"type"`
	Total         int         `json:"total"`
	Truncated     bool        `json:"truncated"`
	AppliedLimit  int         `json:"applied_limit"`
	AppliedOffset int         `json:"applied_offset"`
	Entries       []findEntry `json:"entries"`
}

// findInput is the validated input shared by every find engine.
type findInput struct {
	relPath       string // workspace-relative search path (for the result Path field)
	absPath       string // resolved absolute search root
	pattern       string // raw pattern as given ("" = match everything)
	re            *regexp.Regexp // compiled name regex; nil = match every entry
	entryType     string // findTypeFile / findTypeDirectory / findTypeAny
	maxDepth      int    // positive = component count cap below the root; 0 = unlimited
	ignoreCase    bool
	includeHidden bool
	headLimit     int
	offset        int
}

// findEngineResult is what an engine returns: the full (bounded) entry list
// plus a flag indicating the internal collect ceiling was hit.
type findEngineResult struct {
	entries       []findEntry
	collectCapped bool
}

// findEngine abstracts the search backend so xizhi_find can use fd when it is
// installed and fall back to a pure-Go walker otherwise. Both engines return
// entries in the same findEntry shape; buildFindResult normalizes order,
// pagination and truncation so the final findResult is identical regardless
// of engine.
type findEngine interface {
	search(ctx context.Context, in findInput) (findEngineResult, error)
}

// FindEntries is the full-parameter entry point used by the tool registry: it
// validates inputs, delegates to the resolved find engine (fd when available,
// Go fallback otherwise), and maps the raw entries into the final paginated
// result. An empty or blank pattern matches every entry; maxDepth <= 0 passed
// here is treated as an error only when negative (the registry layer rejects
// an explicit zero via its *int decode, so 0 reaching here means "omitted").
func FindEntries(
	workspaceRoot, relPath, pattern, entryType string,
	maxDepth int,
	ignoreCase, includeHidden bool,
	headLimit, offset int,
) (any, error) {
	return findRun(context.Background(), workspaceRoot, relPath, pattern, entryType, maxDepth, ignoreCase, includeHidden, headLimit, offset)
}

func findRun(
	ctx context.Context,
	workspaceRoot, relPath, pattern, entryType string,
	maxDepth int,
	ignoreCase, includeHidden bool,
	headLimit, offset int,
) (any, error) {
	relPath = normalizePath(relPath)
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("directory not found: %w", err)
		}
		return nil, fmt.Errorf("xizhi_find: stat %q: %w", absPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("xizhi_find: %q is not a directory", relPath)
	}

	if entryType == "" {
		entryType = findTypeAny
	}
	switch entryType {
	case findTypeFile, findTypeDirectory, findTypeAny:
	default:
		return nil, fmt.Errorf("xizhi_find: invalid type %q (must be %q, %q or %q)", entryType, findTypeFile, findTypeDirectory, findTypeAny)
	}
	if maxDepth < 0 {
		return nil, fmt.Errorf("xizhi_find: max_depth must be a positive integer (got %d)", maxDepth)
	}

	if strings.TrimSpace(pattern) == "" {
		pattern = ""
	}
	var re *regexp.Regexp
	if pattern != "" {
		expr := pattern
		if ignoreCase {
			expr = "(?i)" + pattern
		}
		re, err = regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("xizhi_find: invalid regex %q: %w", pattern, err)
		}
	}

	if headLimit <= 0 {
		headLimit = defaultFindHeadLimit
	}
	if offset < 0 {
		offset = 0
	}

	in := findInput{
		relPath:       relPath,
		absPath:       absPath,
		pattern:       pattern,
		re:            re,
		entryType:     entryType,
		maxDepth:      maxDepth,
		ignoreCase:    ignoreCase,
		includeHidden: includeHidden,
		headLimit:     headLimit,
		offset:        offset,
	}

	er, err := defaultFindEngine().search(ctx, in)
	if err != nil {
		return nil, err
	}
	return buildFindResult(in, er), nil
}

// buildFindResult maps an engine's raw entries into the final, paginated
// findResult. It sorts entries by path (fd traverses in parallel and the Go
// walker is directory-ordered; sorting guarantees the two engines produce
// identical output), then applies the offset/head_limit window.
func buildFindResult(in findInput, er findEngineResult) findResult {
	res := findResult{
		Path:       in.relPath,
		Pattern:    in.pattern,
		Type:       in.entryType,
		Entries:    []findEntry{},
	}

	sort.SliceStable(er.entries, func(i, j int) bool {
		return er.entries[i].Path < er.entries[j].Path
	})

	total := len(er.entries)
	start, end := pageWindow(total, in.offset, in.headLimit)
	res.Entries = append(res.Entries, er.entries[start:end]...)
	res.Total = total
	res.Truncated = in.offset+in.headLimit < total || er.collectCapped

	res.AppliedLimit = in.headLimit
	res.AppliedOffset = in.offset
	return res
}
