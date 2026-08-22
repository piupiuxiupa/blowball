package xizhi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGrepFiles_SingleFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "notes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes", "a.txt"), []byte("alpha\nSELECT 1\nbeta\nSELECT 2\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes", "b.txt"), []byte("SELECT 3\n"), 0o644))

	// A regular-file path searches just that file; the result shape matches
	// directory mode (Path echoes the request, file is the basename).
	res, err := GrepFiles(root, "notes/a.txt", "SELECT", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Equal(t, "notes/a.txt", got.Path)
	require.Len(t, got.Matches, 2)
	assert.Equal(t, "a.txt", got.Matches[0].File)
	assert.Equal(t, 2, got.Matches[0].LineNumber)
	assert.Equal(t, "SELECT 1", got.Matches[0].Line)
	assert.Equal(t, "a.txt", got.Matches[1].File)
	assert.Equal(t, 4, got.Matches[1].LineNumber)
	assert.Equal(t, 1, got.TotalFiles)
}

func TestGrepFiles_SingleFile_HiddenRootStillSearched(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("secret=1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "other.txt"), []byte("secret=2\n"), 0o644))

	// Hidden filtering applies to entries discovered by traversal, never to
	// the explicitly targeted file itself.
	res, err := GrepFiles(root, ".env", "secret", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, ".env", got.Matches[0].File)
}

func TestGrepFiles_SingleFile_GlobMismatchIsEmpty(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b.txt"), []byte("hit\n"), 0o644))

	// glob stays a basename post-filter: a non-matching basename yields an
	// empty result, not an error.
	res, err := GrepFiles(root, "a/b.txt", "hit", "*.go", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Empty(t, got.Matches)
	assert.Equal(t, 0, got.TotalFiles)
}

func TestGrepFiles_SingleFile_BinaryIsEmpty(t *testing.T) {
	root := t.TempDir()
	bin := append([]byte("func Foo\n"), 0, 0, 0) // NUL bytes -> binary
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin.dat"), bin, 0o644))

	res, err := GrepFiles(root, "bin.dat", "Foo", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Empty(t, got.Matches)
}

func TestGrepFiles_SingleFile_NotFound(t *testing.T) {
	root := t.TempDir()
	_, err := GrepFiles(root, "missing.txt", "x", "", false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGrepFiles_RegexMatches(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("package main\nfunc Foo() {}\nfunc Bar() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "b.go"), []byte("func Foo() int { return 1 }\n"), 0o644))

	res, err := GrepFiles(root, "src", `func Foo\(`, "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Equal(t, "src", got.Path)
	assert.Equal(t, `func Foo\(`, got.Pattern)
	require.Len(t, got.Matches, 2)

	// file is relative to the search path.
	files := []string{got.Matches[0].File, got.Matches[1].File}
	assert.Contains(t, files, "a.go")
	assert.Contains(t, files, "b.go")

	// Line numbers and line text for a.go (line 2).
	var aMatch grepMatch
	for _, m := range got.Matches {
		if m.File == "a.go" {
			aMatch = m
		}
	}
	assert.Equal(t, 2, aMatch.LineNumber)
	assert.Equal(t, "func Foo() {}", aMatch.Line)
}

func TestGrepFiles_GlobFilter(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("TODO fix\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.py"), []byte("TODO fix\n"), 0o644))

	res, err := GrepFiles(root, ".", "TODO", "*.go", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "a.go", got.Matches[0].File)
	assert.Equal(t, "TODO fix", got.Matches[0].Line)
}

func TestGrepFiles_IgnoreCase(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("Error\nERROR\nerror\nok\n"), 0o644))

	res, err := GrepFiles(root, ".", "error", "", true, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{got.Matches[0].LineNumber, got.Matches[1].LineNumber, got.Matches[2].LineNumber})
	assert.True(t, got.IgnoreCase)
}

func TestGrepFiles_ContextLines(t *testing.T) {
	root := t.TempDir()
	content := "line1\nline2\ndef main():\nline4\nline5\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.py"), []byte(content), 0o644))

	res, err := GrepFiles(root, ".", "def main", "", false, false, 2, 2)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 1)
	m := got.Matches[0]
	assert.Equal(t, 3, m.LineNumber)
	assert.Equal(t, []string{"line1", "line2"}, m.ContextBefore)
	assert.Equal(t, []string{"line4", "line5"}, m.ContextAfter)
}

func TestGrepFiles_BinarySkipped(t *testing.T) {
	root := t.TempDir()
	bin := append([]byte("func Foo\n"), 0, 0, 0) // NUL bytes -> binary
	require.NoError(t, os.WriteFile(filepath.Join(root, "bin.dat"), bin, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("func Foo\n"), 0o644))

	res, err := GrepFiles(root, ".", "Foo", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "a.go", got.Matches[0].File, "binary file should be skipped, no error")
}

func TestGrepFiles_DotBlowballRejected(t *testing.T) {
	root := t.TempDir()
	_, err := GrepFiles(root, ".blowball/secrets", "x", "", false, false, 0, 0)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestGrepFiles_ResultCapTruncates(t *testing.T) {
	root := t.TempDir()
	// A single file with >maxGrepMatches occurrences of "x".
	var content []byte
	for i := 0; i < maxGrepMatches+50; i++ {
		content = append(content, 'x', '\n')
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.txt"), content, 0o644))

	res, err := GrepFiles(root, ".", "x", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Len(t, got.Matches, maxGrepMatches)
	assert.True(t, got.Truncated, "truncated flag must be set when the match cap is hit")
}

func TestGrepFiles_InvalidRegex(t *testing.T) {
	root := t.TempDir()
	_, err := GrepFiles(root, ".", "[", "", false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid regex")
}

func TestGrepFiles_DotPathSearchesRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("import \"fmt\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "util.go"), []byte("import \"os\"\n"), 0o644))

	// "." explicitly means the workspace root; default include_hidden=false.
	res, err := GrepFiles(root, ".", "import", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	files := []string{got.Matches[0].File, got.Matches[1].File}
	assert.Contains(t, files, "main.go")
	assert.Contains(t, files, "pkg/util.go")
}

func TestGrepFiles_EmptyPathRejected(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("import \"fmt\"\n"), 0o644))

	// An empty path is an error, not a fallback to the workspace root.
	_, err := GrepFiles(root, "", "import", "", false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is required")

	// A whitespace-only path is equally rejected.
	_, err = GrepFiles(root, "   ", "import", "", false, false, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path is required")
}

func TestGrepFiles_HiddenExcludedByDefault(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("secret\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("secret\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "visible.txt"), []byte("secret\n"), 0o644))

	res, err := GrepFiles(root, ".", "secret", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "visible.txt", got.Matches[0].File, "hidden files and dirs excluded by default")

	// include_hidden surfaces them.
	res, err = GrepFiles(root, ".", "secret", "", false, true, 0, 0)
	require.NoError(t, err)
	got = res.(grepResult)
	files := make([]string, len(got.Matches))
	for i, m := range got.Matches {
		files[i] = m.File
	}
	assert.Contains(t, files, "visible.txt")
	assert.Contains(t, files, ".env")
	assert.Contains(t, files, ".git/config")
}

func TestGrepFiles_SymlinksNotFollowed(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir() // outside the workspace root
	require.NoError(t, os.WriteFile(filepath.Join(target, "outside.go"), []byte("func Foo\n"), 0o644))
	// A symlink inside the workspace pointing outside.
	require.NoError(t, os.Symlink(target, filepath.Join(root, "link")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "real.go"), []byte("func Foo\n"), 0o644))

	res, err := GrepFiles(root, ".", "Foo", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "real.go", got.Matches[0].File, "symlinked dir must not be followed")
}

func TestGrepFiles_EmptyPatternRejected(t *testing.T) {
	root := t.TempDir()
	_, err := GrepFiles(root, ".", "", "", false, false, 0, 0)
	require.Error(t, err)
}

func TestGrepFiles_ViaRegistry_Execute(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("func Foo() {}\n"), 0o644))

	r := newTestRegistry(t)
	RegisterAll(r, root, testXizhiConfig())

	spec, ok := r.Get(NameGrep)
	require.True(t, ok)

	args, err := json.Marshal(grepArgs{Path: ".", Pattern: `func Foo\(`})
	require.NoError(t, err)

	res, err := spec.Execute(t.Context(), args)
	require.NoError(t, err)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"file":"a.go"`)
	assert.Contains(t, string(b), `"line_number":1`)
}

func TestGrepFiles_LineTruncated(t *testing.T) {
	root := t.TempDir()
	// A single line longer than maxGrepLineRunes that still matches.
	long := make([]byte, 0, maxGrepLineRunes*2+5)
	long = append(long, 'A')
	for i := 0; i < maxGrepLineRunes*2; i++ {
		long = append(long, 'B')
	}
	long = append(long, '\n')
	require.NoError(t, os.WriteFile(filepath.Join(root, "long.txt"), long, 0o644))

	res, err := GrepFiles(root, ".", "A", "", false, false, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 1)
	assert.LessOrEqual(t, len([]rune(got.Matches[0].Line)), maxGrepLineRunes)
}

func TestGrepSearch_FilesWithMatches(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("func Foo\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte("func Foo\nfunc Bar\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "c.go"), []byte("nope\n"), 0o644))

	res, err := GrepSearch(root, ".", "Foo", "", false, false, 0, 0, outputModeFiles, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Equal(t, outputModeFiles, got.Mode)
	assert.Equal(t, []string{"a.go", "b.go"}, got.Files)
	assert.Equal(t, 2, got.TotalFiles)
	assert.False(t, got.Truncated)
	assert.Equal(t, defaultGrepHeadLimit, got.AppliedLimit)
	assert.Equal(t, 0, got.AppliedOffset)
}

func TestGrepSearch_Count(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("Foo\nFoo\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte("Foo\n"), 0o644))

	res, err := GrepSearch(root, ".", "Foo", "", false, false, 0, 0, outputModeCount, 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Equal(t, outputModeCount, got.Mode)
	assert.ElementsMatch(t, []grepCount{{File: "a.go", Count: 2}, {File: "b.go", Count: 1}}, got.Counts)
	assert.Equal(t, 2, got.TotalFiles)
}

func TestGrepSearch_ContentIsDefault(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("func Foo\n"), 0o644))

	// An empty/unknown output_mode falls back to content (backward compatible).
	res, err := GrepSearch(root, ".", "Foo", "", false, false, 0, 0, "", 0, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Equal(t, outputModeContent, got.Mode)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, "a.go", got.Matches[0].File)
}

func TestGrepSearch_HeadLimitOffsetPaginates(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("m\nm\nm\nm\nm\n"), 0o644)) // 5 matches

	// Page 1: first 2 matches.
	res, err := GrepSearch(root, ".", "m", "", false, false, 0, 0, outputModeContent, 2, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	require.Len(t, got.Matches, 2)
	assert.Equal(t, []int{1, 2}, []int{got.Matches[0].LineNumber, got.Matches[1].LineNumber})
	assert.Equal(t, 2, got.AppliedLimit)
	assert.Equal(t, 0, got.AppliedOffset)
	assert.Equal(t, 1, got.TotalFiles)
	assert.True(t, got.Truncated, "3 more matches remain")

	// Page 2: matches 3 and 4.
	res, err = GrepSearch(root, ".", "m", "", false, false, 0, 0, outputModeContent, 2, 2)
	require.NoError(t, err)
	got = res.(grepResult)
	require.Len(t, got.Matches, 2)
	assert.Equal(t, []int{3, 4}, []int{got.Matches[0].LineNumber, got.Matches[1].LineNumber})
	assert.Equal(t, 2, got.AppliedOffset)
	assert.True(t, got.Truncated, "1 more match remains")

	// Page 3: the final match — no truncation.
	res, err = GrepSearch(root, ".", "m", "", false, false, 0, 0, outputModeContent, 2, 4)
	require.NoError(t, err)
	got = res.(grepResult)
	require.Len(t, got.Matches, 1)
	assert.Equal(t, 5, got.Matches[0].LineNumber)
	assert.False(t, got.Truncated)
}

func TestGrepSearch_FilesModeHeadLimitPaginates(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name+".go"), []byte("hit\n"), 0o644))
	}

	// head_limit applies to file paths in files_with_matches mode.
	res, err := GrepSearch(root, ".", "hit", "", false, false, 0, 0, outputModeFiles, 2, 0)
	require.NoError(t, err)
	got := res.(grepResult)
	assert.Len(t, got.Files, 2)
	assert.Equal(t, 4, got.TotalFiles)
	assert.True(t, got.Truncated)
}
