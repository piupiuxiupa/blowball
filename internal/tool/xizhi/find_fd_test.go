package xizhi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFind_FdGoParity asserts the fd engine and the Go fallback produce
// identical findResult for the same input across type filters, depth caps,
// case-insensitivity, hidden inclusion, and pagination. It is skipped when fd
// is not installed so CI without fd stays green (mirrors
// TestGrep_RgGoParity).
func TestFind_FdGoParity(t *testing.T) {
	fdPath, err := exec.LookPath("fd")
	if err != nil {
		t.Skip("fd not installed; skipping fd/Go parity test")
	}
	fd := fdFindEngine{fdPath: fdPath}
	goEng := goFindEngine{}

	fixtures := []struct {
		name          string
		files         map[string]string
		symlinks      map[string]string // link name -> target (workspace-relative)
		target        string            // search path relative to root (default ".")
		pattern       string
		entryType     string
		maxDepth      int
		ignoreCase    bool
		includeHidden bool
		headLimit     int
		offset        int
		emptyOk       bool // fixture intentionally matches nothing
	}{
		{
			name: "regex name match",
			files: map[string]string{
				"src/a.go":         "x",
				"src/nested/b.go":  "x",
				"src/c.txt":        "x",
				"docs/guide.md":    "x",
				"testdata/old.txt": "x",
			},
			pattern: `\.go$`,
		},
		{
			name: "empty pattern matches everything",
			files: map[string]string{
				"README.md":     "x",
				"docs/a.md":     "x",
				"src/deep/b.go": "x",
			},
			pattern: "",
		},
		{
			name:      "type file",
			files:     map[string]string{"testdir/inner.txt": "x", "tests.txt": "x"},
			pattern:   `test`,
			entryType: findTypeFile,
		},
		{
			name:      "type directory",
			files:     map[string]string{"testdir/inner.txt": "x", "tests.txt": "x"},
			pattern:   `test`,
			entryType: findTypeDirectory,
		},
		{
			name: "max depth",
			files: map[string]string{
				"a/b/c/d.txt": "x",
				"a/one.txt":   "x",
			},
			pattern:  "",
			maxDepth: 2,
		},
		{
			name:       "ignore case",
			files:      map[string]string{"README.md": "x", "notes.txt": "x"},
			pattern:    `readme`,
			ignoreCase: true,
		},
		{
			name: "case sensitive default",
			files: map[string]string{
				"README.md": "x",
				"notes.txt": "x",
			},
			pattern: `readme`,
			emptyOk: true,
		},
		{
			name: "include hidden",
			files: map[string]string{
				".git/config": "x",
				".env":        "x",
				"visible.txt": "x",
			},
			pattern:       "",
			includeHidden: true,
		},
		{
			name: "hidden excluded by default",
			files: map[string]string{
				".git/config": "x",
				".env":        "x",
				"visible.txt": "x",
			},
			pattern: "",
		},
		{
			name: "hidden search root is searched",
			files: map[string]string{
				".hid/a.txt":     "x",
				".hid/sub/b.txt": "x",
				"vis.txt":        "x",
			},
			target:  ".hid",
			pattern: "",
		},
		{
			name: "symlinks excluded",
			files: map[string]string{
				"real.txt":  "x",
				"realdir/x": "x",
			},
			symlinks: map[string]string{
				"link.txt": "real.txt",
				"linkdir":  "realdir",
			},
			pattern: "",
		},
		{
			name: "pagination window",
			files: map[string]string{
				"f001.txt": "x", "f002.txt": "x", "f003.txt": "x",
				"f004.txt": "x", "f005.txt": "x",
			},
			pattern:   `\.txt$`,
			headLimit: 2,
			offset:    2,
		},
		{
			name: "subdirectory target",
			files: map[string]string{
				"src/a.go":        "x",
				"src/nested/b.go": "x",
				"other/c.go":      "x",
			},
			target:  "src",
			pattern: `\.go$`,
		},
		{
			name: "pattern matching basename only",
			files: map[string]string{
				"a/b/c.txt": "x",
			},
			pattern: `b/c`,
			emptyOk: true,
		},
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			root := t.TempDir()
			for path, content := range f.files {
				abs := filepath.Join(root, filepath.FromSlash(path))
				require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
				require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
			}
			for link, target := range f.symlinks {
				require.NoError(t, os.Symlink(filepath.Join(root, filepath.FromSlash(target)), filepath.Join(root, link)))
			}

			target := f.target
			if target == "" {
				target = "."
			}
			absTarget, err := validatePath(root, target)
			require.NoError(t, err)
			entryType := f.entryType
			if entryType == "" {
				entryType = findTypeAny
			}
			var re *regexp.Regexp
			if f.pattern != "" {
				expr := f.pattern
				if f.ignoreCase {
					expr = "(?i)" + f.pattern
				}
				re = regexp.MustCompile(expr)
			}
			headLimit := f.headLimit
			if headLimit <= 0 {
				headLimit = defaultFindHeadLimit
			}
			in := findInput{
				relPath:       target,
				absPath:       absTarget,
				pattern:       f.pattern,
				re:            re,
				entryType:     entryType,
				maxDepth:      f.maxDepth,
				ignoreCase:    f.ignoreCase,
				includeHidden: f.includeHidden,
				headLimit:     headLimit,
				offset:        f.offset,
			}

			fdRes, err := fd.search(context.Background(), in)
			require.NoError(t, err, "fd engine failed")
			goRes, err := goEng.search(context.Background(), in)
			require.NoError(t, err, "go engine failed")

			fdOut := buildFindResult(in, fdRes)
			goOut := buildFindResult(in, goRes)
			if !f.emptyOk {
				assert.NotEmpty(t, goOut.Entries, "fixture should match something (otherwise parity is vacuous)")
			}
			assert.Equal(t, goOut, fdOut, "fd and Go engines must produce identical findResult")
		})
	}
}

// TestResolveFindEngine_FdfindAlias pins the Debian/Ubuntu fd-find binary
// fallback: with a PATH that has no fd but does have fdfind, the fd engine
// resolves against fdfind; with neither, the Go engine is used. It calls the
// uncached resolver so the process-wide defaultFindEngine is unaffected.
func TestResolveFindEngine_FdfindAlias(t *testing.T) {
	binDir := t.TempDir()
	fdfind := filepath.Join(binDir, "fdfind")
	script := "#!/bin/sh\nexit 0\n"
	require.NoError(t, os.WriteFile(fdfind, []byte(script), 0o755))

	t.Run("fdfind alias", func(t *testing.T) {
		t.Setenv("PATH", binDir)
		eng := resolveFindEngine()
		fdEng, ok := eng.(fdFindEngine)
		require.True(t, ok, "expected fdFindEngine via the fdfind alias, got %T", eng)
		assert.Equal(t, fdfind, fdEng.fdPath)
	})
	t.Run("no fd binary falls back to Go", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		eng := resolveFindEngine()
		_, ok := eng.(goFindEngine)
		assert.True(t, ok, "expected goFindEngine fallback, got %T", eng)
	})
}
