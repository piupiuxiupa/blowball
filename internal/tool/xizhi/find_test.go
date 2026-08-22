package xizhi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goFind runs the pure-Go engine over a validated findInput, mirroring
// findRun's normalization, so engine behavior is testable independently of
// whether fd is installed (FindEntries resolves whichever engine is present).
func goFind(t *testing.T, root, relPath, pattern, entryType string, maxDepth int, ignoreCase, includeHidden bool, headLimit, offset int) (findResult, error) {
	t.Helper()
	if relPath == "" {
		relPath = "."
	}
	absPath, err := validatePath(root, relPath)
	if err != nil {
		return findResult{}, err
	}
	if entryType == "" {
		entryType = findTypeAny
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
		re = regexp.MustCompile(expr)
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
	er, err := goFindEngine{}.search(context.Background(), in)
	if err != nil {
		return findResult{}, err
	}
	return buildFindResult(in, er), nil
}

// writeFindTree creates a deterministic fixture tree under root:
//
//	src/a.go, src/nested/b.go, src/c.txt
//	testdata/notes.txt
//	docs/guide.md
//	README.md
//	.hidden.txt, .git/config
func writeFindTree(t *testing.T, root string) {
	t.Helper()
	mustWrite := func(rel string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte("x"), 0o644))
	}
	mustWrite("src/a.go")
	mustWrite("src/nested/b.go")
	mustWrite("src/c.txt")
	mustWrite("testdata/notes.txt")
	mustWrite("docs/guide.md")
	mustWrite("README.md")
	mustWrite(".hidden.txt")
	mustWrite(".git/config")
}

func findPaths(res findResult) []string {
	paths := make([]string, 0, len(res.Entries))
	for _, e := range res.Entries {
		paths = append(paths, e.Path)
	}
	return paths
}

func TestFind_RegexNameMatch(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	res, err := goFind(t, root, ".", `\.go$`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"src/a.go", "src/nested/b.go"}, findPaths(res))
	for _, e := range res.Entries {
		assert.Equal(t, findTypeFile, e.Type)
	}
	assert.Equal(t, 2, res.Total)
	assert.False(t, res.Truncated)
}

func TestFind_TypeFilter(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	// any: both files and directories carry their type
	res, err := goFind(t, root, ".", `test`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []findEntry{{Path: "testdata", Type: findTypeDirectory}}, res.Entries)

	// directory only (every directory in the tree, at any depth)
	res, err = goFind(t, root, ".", "", findTypeDirectory, 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"docs", "src", "src/nested", "testdata"}, findPaths(res))

	// file only: directories are excluded from the results but still
	// descended into (matches live under them)
	res, err = goFind(t, root, ".", `\.txt$`, findTypeFile, 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"src/c.txt", "testdata/notes.txt"}, findPaths(res))
}

func TestFind_EmptyPatternMatchesEverything(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	want := []string{
		"README.md",
		"docs",
		"docs/guide.md",
		"src",
		"src/a.go",
		"src/c.txt",
		"src/nested",
		"src/nested/b.go",
		"testdata",
		"testdata/notes.txt",
	}
	for _, pattern := range []string{"", "   "} {
		res, err := goFind(t, root, ".", pattern, "", 0, false, false, 0, 0)
		require.NoError(t, err)
		assert.Equal(t, want, findPaths(res), "pattern %q", pattern)
	}
}

func TestFind_PatternMatchesBasenameNotPath(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	// "src/a" spans two path components; basename matching must not hit it.
	res, err := goFind(t, root, ".", `src/a`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, res.Entries)

	res, err = goFind(t, root, ".", `nested`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"src/nested"}, findPaths(res))
}

func TestFind_IgnoreCase(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	res, err := goFind(t, root, ".", `readme`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, res.Entries)

	res, err = goFind(t, root, ".", `readme`, "", 0, true, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"README.md"}, findPaths(res))
}

func TestFind_IncludeHidden(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	res, err := goFind(t, root, ".", `^\.`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, res.Entries)

	res, err = goFind(t, root, ".", `^\.`, "", 0, false, true, 0, 0)
	require.NoError(t, err)
	// The pattern matches entry NAMES: only the dot-prefixed names themselves
	// hit (config inside .git does not — its basename is "config"). Descent
	// into hidden directories is covered by the fd parity fixtures.
	assert.Equal(t, []string{".git", ".hidden.txt"}, findPaths(res))
}

func TestFind_MaxDepth(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	// Depth counts components below the search root: src/nested/b.go is 3.
	for _, tc := range []struct {
		maxDepth int
		want     []string
	}{
		{1, []string{}},
		{2, []string{}},
		{3, []string{"src/nested/b.go"}},
	} {
		res, err := goFind(t, root, ".", `b\.go$`, "", tc.maxDepth, false, false, 0, 0)
		require.NoError(t, err)
		assert.Equal(t, tc.want, findPaths(res), "max_depth %d", tc.maxDepth)
	}

	// Empty pattern + max_depth 1 lists immediate children only (both kinds).
	res, err := goFind(t, root, ".", "", "", 1, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"README.md", "docs", "src", "testdata"}, findPaths(res))
}

func TestFind_FromSubdirectory(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	res, err := goFind(t, root, "src", `\.go$`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.go", "nested/b.go"}, findPaths(res))
	assert.Equal(t, "src", res.Path)
}

func TestFind_SymlinksExcluded(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)
	require.NoError(t, os.Symlink(filepath.Join(root, "README.md"), filepath.Join(root, "link.md")))
	require.NoError(t, os.Symlink(filepath.Join(root, "docs"), filepath.Join(root, "linkdir")))

	res, err := goFind(t, root, ".", `^link`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, res.Entries)
}

func TestFind_ViaFindEntries_DefaultsToRoot(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	// Empty path means the workspace root; validation errors are
	// engine-independent (they fire before dispatch).
	resAny, err := FindEntries(root, "", `notes`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	res := resAny.(findResult)
	assert.Equal(t, ".", res.Path)
	assert.Equal(t, []string{"testdata/notes.txt"}, findPaths(res))
}

func TestFind_PathNotDirectory(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	_, err := FindEntries(root, "README.md", "", "", 0, false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestFind_PathMissing(t *testing.T) {
	root := t.TempDir()
	_, err := FindEntries(root, "nope", "", "", 0, false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory not found")
}

func TestFind_ReservedNamespaceRejected(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".blowball", "mcp"), 0o755))

	_, err := FindEntries(root, ".blowball/mcp", "", "", 0, false, false, 0, 0)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestFind_PathOutsideWorkspaceRejected(t *testing.T) {
	root := t.TempDir()
	_, err := FindEntries(root, "../../etc", "", "", 0, false, false, 0, 0)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestFind_InvalidRegex(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	_, err := FindEntries(root, ".", `*\.go`, "", 0, false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid regex")
}

func TestFind_InvalidType(t *testing.T) {
	root := t.TempDir()
	_, err := FindEntries(root, ".", "", "folder", 0, false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `invalid type "folder"`)
}

func TestFind_NegativeMaxDepthRejected(t *testing.T) {
	root := t.TempDir()
	_, err := FindEntries(root, ".", "", "", -1, false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_depth must be a positive integer")
}

func TestFind_PaginationWindowAndTruncated(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 205; i++ {
		name := fmt.Sprintf("f%03d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644))
	}

	// Default window: 200 of 205, truncated.
	res, err := goFind(t, root, ".", `\.txt$`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, 200, len(res.Entries))
	assert.Equal(t, 205, res.Total)
	assert.True(t, res.Truncated)
	assert.Equal(t, 200, res.AppliedLimit)
	assert.Equal(t, 0, res.AppliedOffset)
	assert.Equal(t, "f000.txt", res.Entries[0].Path)

	// Second page: the remaining 5, not truncated.
	res, err = goFind(t, root, ".", `\.txt$`, "", 0, false, false, 0, 200)
	require.NoError(t, err)
	assert.Equal(t, 5, len(res.Entries))
	assert.Equal(t, 205, res.Total)
	assert.False(t, res.Truncated)
	assert.Equal(t, 200, res.AppliedOffset)
	assert.Equal(t, "f200.txt", res.Entries[0].Path)

	// An explicit larger limit returns everything in one page.
	res, err = goFind(t, root, ".", `\.txt$`, "", 0, false, false, 1000, 0)
	require.NoError(t, err)
	assert.Equal(t, 205, len(res.Entries))
	assert.False(t, res.Truncated)
	assert.Equal(t, 1000, res.AppliedLimit)
}

func TestFind_CollectCapSetsTruncated(t *testing.T) {
	in := findInput{relPath: ".", pattern: "", entryType: findTypeAny, headLimit: defaultFindHeadLimit, offset: 0}
	er := findEngineResult{
		entries: []findEntry{
			{Path: "a", Type: findTypeFile},
			{Path: "b", Type: findTypeFile},
		},
		collectCapped: true,
	}
	res := buildFindResult(in, er)
	assert.True(t, res.Truncated, "collect cap must set truncated even within the page window")
	assert.Equal(t, 2, res.Total)
}

func TestFind_ResultEntriesAlwaysPresent(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	res, err := goFind(t, root, ".", `zzz-no-match`, "", 0, false, false, 0, 0)
	require.NoError(t, err)
	// Entries must serialize as [], not null.
	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"entries":[]`)
	assert.Equal(t, 0, res.Total)
}

func TestFind_ViaRegistry_Execute(t *testing.T) {
	root := t.TempDir()
	writeFindTree(t, root)

	r := newTestRegistry(t)
	RegisterAll(r, root, testXizhiConfig())

	spec, ok := r.Get(NameFind)
	require.True(t, ok)

	args, err := json.Marshal(findArgs{Path: "src", Pattern: `\.go$`})
	require.NoError(t, err)

	res, err := spec.Execute(t.Context(), args)
	require.NoError(t, err)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"path":"src"`)
	assert.Contains(t, string(b), `"entries":[{"path":"a.go","type":"file"}`)

	// An explicit max_depth of 0 is rejected at the registry layer.
	zero := 0
	args, err = json.Marshal(findArgs{Path: ".", MaxDepth: &zero})
	require.NoError(t, err)
	_, err = spec.Execute(t.Context(), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_depth must be a positive integer")

	neg := -2
	args, err = json.Marshal(findArgs{Path: ".", MaxDepth: &neg})
	require.NoError(t, err)
	_, err = spec.Execute(t.Context(), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_depth must be a positive integer")
}
