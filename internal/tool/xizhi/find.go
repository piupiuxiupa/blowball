package xizhi

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Entry-type filter values accepted by xizhi_find's `type` parameter. They are
// re-exported as SearchType* so non-agent callers of RunSearch (the workspace
// search REST handler) can map their own wire values onto the engine's.
const (
	findTypeFile      = "file"
	findTypeDirectory = "directory"
	findTypeAny       = "any"
)

// SearchTypeFile / SearchTypeDirectory / SearchTypeAny are the exported spellings
// of the entry-type filter values RunSearch accepts in PreparedSearch.EntryType.
const (
	SearchTypeFile      = findTypeFile
	SearchTypeDirectory = findTypeDirectory
	SearchTypeAny       = findTypeAny
)

// defaultFindHeadLimit bounds the number of entries returned when the caller
// does not request pagination (the same default xizhi_grep uses).
const defaultFindHeadLimit = 200

// SearchDefaultHeadLimit re-exports defaultFindHeadLimit for callers outside
// the package (the workspace search REST endpoint) that must default their own
// head_limit parameter to the same value.
const SearchDefaultHeadLimit = defaultFindHeadLimit

// maxFindCollect is a memory-safety ceiling on how many entries an engine
// collects before stopping, mirroring maxGrepCollect. Hitting it sets the
// result's truncated flag and makes total a lower bound.
const maxFindCollect = 10000

// SearchEntry is one matched workspace entry. Path is relative to the search
// root (slash-separated); Type is "file" or "directory".
type SearchEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// findEntry is the engine-internal spelling of SearchEntry (kept as an alias so
// the engines and their tests reference one shape).
type findEntry = SearchEntry

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
	relPath       string         // workspace-relative search path (for the result Path field)
	absPath       string         // resolved absolute search root
	pattern       string         // raw pattern as given ("" = match everything)
	re            *regexp.Regexp // compiled name regex; nil = match every entry
	entryType     string         // findTypeFile / findTypeDirectory / findTypeAny
	maxDepth      int            // positive = component count cap below the root; 0 = unlimited
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

// PreparedSearch is a fully validated search request ready for execution: the
// execution half of the find pipeline (RunSearch) trusts these values — the
// caller has already resolved/validated the search root, defaulted the
// pagination window, and compiled the pattern. Both entry points build one:
// findRun applies the agent-side validation (ValidatePath, .blowball reserved,
// xizhi_find error wording) and the workspace search REST handler applies its
// own (ValidatePathAllowReserved, substring→literal escaping, its own error
// mapping) — which is why the two paths can share the engines without sharing
// a path policy.
type PreparedSearch struct {
	RelPath string // search root as the caller knows it (envelope-only; "." for the root)
	AbsPath string // resolved absolute search root
	// Pattern is the ENGINE-facing pattern string ("" = match everything). The
	// fd engine consumes it VERBATIM as an fd regex argument — callers with
	// non-regex input (e.g. a REST substring search) must pass the escaped
	// form (regexp.QuoteMeta) here, not the raw user input; the Go engine
	// ignores it and matches with Re instead.
	Pattern       string         `json:"-"`
	Re            *regexp.Regexp // compiled name regex; nil = match every entry
	EntryType     string         // findTypeFile / findTypeDirectory / findTypeAny
	MaxDepth      int            // positive = component count cap below the root; 0 = unlimited
	IgnoreCase    bool
	IncludeHidden bool
	HeadLimit     int
	Offset        int
}

// SearchPage is the typed, paginated result of a RunSearch execution: the
// engine's matched entries sorted by path (the stable pagination backbone)
// and sliced to the offset/head_limit window, plus the window metadata. Unlike
// findResult it carries no JSON envelope — each caller (agent tool result,
// REST response DTO) wraps it in its own shape.
type SearchPage struct {
	Entries       []SearchEntry
	Total         int // matched entries before pagination (a lower bound when the collect ceiling was hit)
	Truncated     bool
	AppliedLimit  int
	AppliedOffset int
}

// RunSearch is the execution half of the find pipeline: it runs the resolved
// engine (fd when available, the pure-Go walker otherwise) under ctx and maps
// the raw entries into a sorted, paginated SearchPage. Inputs arrive validated
// via PreparedSearch — path policy, type spelling, and defaults are the
// caller's concern. Both engines honor ctx cancellation (fd via
// exec.CommandContext, the Go walker via a per-entry ctx.Err() check), so a
// cancelled or timed-out ctx aborts the search with the ctx error.
func RunSearch(ctx context.Context, ps PreparedSearch) (SearchPage, error) {
	in := ps.toFindInput()
	er, err := defaultFindEngine().search(ctx, in)
	if err != nil {
		return SearchPage{}, err
	}
	return buildSearchPage(in, er), nil
}

// toFindInput converts a PreparedSearch into the internal engine input.
func (ps PreparedSearch) toFindInput() findInput {
	return findInput{
		relPath:       ps.RelPath,
		absPath:       ps.AbsPath,
		pattern:       ps.Pattern,
		re:            ps.Re,
		entryType:     ps.EntryType,
		maxDepth:      ps.MaxDepth,
		ignoreCase:    ps.IgnoreCase,
		includeHidden: ps.IncludeHidden,
		headLimit:     ps.HeadLimit,
		offset:        ps.Offset,
	}
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

	page, err := RunSearch(ctx, PreparedSearch{
		RelPath:       relPath,
		AbsPath:       absPath,
		Pattern:       pattern,
		Re:            re,
		EntryType:     entryType,
		MaxDepth:      maxDepth,
		IgnoreCase:    ignoreCase,
		IncludeHidden: includeHidden,
		HeadLimit:     headLimit,
		Offset:        offset,
	})
	if err != nil {
		return nil, err
	}
	return findResult{
		Path:          relPath,
		Pattern:       pattern,
		Type:          entryType,
		Total:         page.Total,
		Truncated:     page.Truncated,
		AppliedLimit:  page.AppliedLimit,
		AppliedOffset: page.AppliedOffset,
		Entries:       page.Entries,
	}, nil
}

// buildFindResult wraps a built SearchPage into the agent-facing findResult
// envelope, echoing the caller's search-root/pattern/type inputs alongside the
// page. It preserves the pre-split behavior: the sort/paginate work now lives
// in buildSearchPage, which both this wrapper and RunSearch share.
func buildFindResult(in findInput, er findEngineResult) findResult {
	page := buildSearchPage(in, er)
	return findResult{
		Path:          in.relPath,
		Pattern:       in.pattern,
		Type:          in.entryType,
		Total:         page.Total,
		Truncated:     page.Truncated,
		AppliedLimit:  page.AppliedLimit,
		AppliedOffset: page.AppliedOffset,
		Entries:       page.Entries,
	}
}

// buildSearchPage maps an engine's raw entries into the final, paginated
// SearchPage. It sorts entries by path (fd traverses in parallel and the Go
// walker is directory-ordered; sorting guarantees the two engines produce
// identical output), then applies the offset/head_limit window.
func buildSearchPage(in findInput, er findEngineResult) SearchPage {
	page := SearchPage{Entries: []SearchEntry{}}

	sort.SliceStable(er.entries, func(i, j int) bool {
		return er.entries[i].Path < er.entries[j].Path
	})

	total := len(er.entries)
	start, end := pageWindow(total, in.offset, in.headLimit)
	page.Entries = append(page.Entries, er.entries[start:end]...)
	page.Total = total
	page.Truncated = in.offset+in.headLimit < total || er.collectCapped

	page.AppliedLimit = in.headLimit
	page.AppliedOffset = in.offset
	return page
}
