package xizhi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrep_RgGoParity asserts the ripgrep engine and the Go fallback produce
// byte-identical grepResult for the same input across modes, glob filters,
// case-insensitivity, context lines, and pagination. It is skipped when rg is
// not installed so CI without ripgrep stays green.
func TestGrep_RgGoParity(t *testing.T) {
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("ripgrep not installed; skipping rg/Go parity test")
	}
	rg := rgGrepEngine{rgPath: rgPath}
	goEng := goGrepEngine{}

	fixtures := []struct {
		name          string
		files         map[string]string
		target        string // search path relative to root (default "."); may name a single file
		pattern       string
		glob          string
		ignoreCase    bool
		includeHidden bool
		ctxBefore     int
		ctxAfter      int
		outputMode    string
		headLimit     int
		offset        int
	}{
		{
			name: "content basic",
			files: map[string]string{
				"a.go": "func Foo() {}\nfunc Bar() {}\n",
				"b.go": "func Foo() int { return 1 }\n",
			},
			pattern: `func Foo\(`, outputMode: outputModeContent,
		},
		{
			name: "single file",
			files: map[string]string{
				"notes/a.txt": "alpha\nSELECT 1\nbeta\nSELECT 2\n",
				"notes/b.txt": "SELECT 3\n",
			},
			target: "notes/a.txt", pattern: "SELECT", outputMode: outputModeContent,
		},
		{
			name: "single file with context",
			files: map[string]string{
				"notes/a.txt": "line1\nline2\ndef main\nline4\nline5\n",
			},
			target: "notes/a.txt", pattern: "def main", ctxBefore: 2, ctxAfter: 2, outputMode: outputModeContent,
		},
		{
			name: "single hidden file",
			files: map[string]string{
				".env":        "secret\n",
				"visible.txt": "secret\n",
			},
			target: ".env", pattern: "secret", outputMode: outputModeContent,
		},
		{
			name: "single file glob mismatch",
			files: map[string]string{
				"a/b.txt": "hit\n",
			},
			target: "a/b.txt", pattern: "hit", glob: "*.go", outputMode: outputModeContent,
		},
		{
			name: "single binary file is skipped",
			files: map[string]string{
				"bin.dat": "func Foo\n\x00\x00\x00",
			},
			target: "bin.dat", pattern: "Foo", outputMode: outputModeContent,
		},
		{
			name: "single file count mode",
			files: map[string]string{
				"notes/a.txt": "Foo\nFoo\n",
				"notes/b.txt": "Foo\n",
			},
			target: "notes/a.txt", pattern: "Foo", outputMode: outputModeCount,
		},
		{
			name: "content with context",
			files: map[string]string{
				"a.py": "line1\nline2\ndef main():\nline4\nline5\n",
			},
			pattern: "def main", ctxBefore: 2, ctxAfter: 2, outputMode: outputModeContent,
		},
		{
			name: "files_with_matches",
			files: map[string]string{
				"a.go": "Foo\n",
				"b.go": "Foo\nBar\n",
				"c.go": "none\n",
			},
			pattern: "Foo", outputMode: outputModeFiles,
		},
		{
			name: "count",
			files: map[string]string{
				"a.go": "Foo\nFoo\n",
				"b.go": "Foo\n",
			},
			pattern: "Foo", outputMode: outputModeCount,
		},
		{
			name: "glob filter",
			files: map[string]string{
				"a.go": "hit\n",
				"b.py": "hit\n",
			},
			pattern: "hit", glob: "*.go", outputMode: outputModeContent,
		},
		{
			name: "ignore case",
			files: map[string]string{
				"a.txt": "Error\nERROR\nerror\nok\n",
			},
			pattern: "error", ignoreCase: true, outputMode: outputModeContent,
		},
		{
			name: "pagination",
			files: map[string]string{
				"a.txt": "m\nm\nm\nm\nm\n",
			},
			pattern: "m", outputMode: outputModeContent, headLimit: 2, offset: 2,
		},
		{
			name: "nested paths",
			files: map[string]string{
				"main.go":     "import \"fmt\"\n",
				"pkg/util.go": "import \"os\"\n",
			},
			pattern: "import", outputMode: outputModeFiles,
		},
		{
			name: "hidden descendants included on request",
			files: map[string]string{
				".env":        "secret\n",
				".git/config": "secret\n",
				"visible.txt": "secret\n",
			},
			pattern: "secret", includeHidden: true, outputMode: outputModeContent,
		},
		{
			name: "directory binary file is skipped",
			files: map[string]string{
				"visible.txt": "hit\n",
				"bin.dat":     "hit\x00\x00\x00",
			},
			pattern: "hit", outputMode: outputModeContent,
		},
		{
			name: "workspace root excludes reserved namespace",
			files: map[string]string{
				".blowball/mcp/config.json": "secret\n",
				".env":                      "secret\n",
				".git/config":               "secret\n",
				"visible.txt":               "secret\n",
			},
			pattern: "secret", includeHidden: true, outputMode: outputModeContent,
		},
		{
			name: "nested reserved-named directory remains searchable",
			files: map[string]string{
				"nested/.blowball/config": "secret\n",
			},
			target: "nested", pattern: "secret", includeHidden: true, outputMode: outputModeContent,
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

			expr := f.pattern
			if f.ignoreCase {
				expr = "(?i)" + f.pattern
			}
			mode := f.outputMode
			if mode == "" {
				mode = outputModeContent
			}
			// Resolve the search target the way grepRun does: "." or a
			// directory walks it; a regular file switches to single-file mode.
			target := f.target
			if target == "" {
				target = "."
			}
			absTarget := filepath.Join(root, filepath.FromSlash(target))
			var searchFile string
			if fi, err := os.Stat(absTarget); err == nil && fi.Mode().IsRegular() {
				searchFile = filepath.Base(absTarget)
			}
			in := grepInput{
				workspaceRoot: root,
				relPath:       target,
				absPath:       absTarget,
				searchFile:    searchFile,
				pattern:       f.pattern,
				glob:          f.glob,
				ignoreCase:    f.ignoreCase,
				includeHidden: f.includeHidden,
				contextBefore: f.ctxBefore,
				contextAfter:  f.ctxAfter,
				outputMode:    mode,
				headLimit:     f.headLimit,
				offset:        f.offset,
				re:            regexp.MustCompile(expr),
			}
			if in.headLimit <= 0 {
				in.headLimit = defaultGrepHeadLimit
			}

			rgRes, err := rg.search(context.Background(), in)
			require.NoError(t, err, "rg engine failed")
			goRes, err := goEng.search(context.Background(), in)
			require.NoError(t, err, "go engine failed")

			rgOut := buildGrepResult(in, rgRes)
			goOut := buildGrepResult(in, goRes)
			assert.Equal(t, goOut, rgOut, "rg and Go engines must produce identical grepResult")
		})
	}
}

func TestRgGrepEngine_CancelledDuringExecution(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep not installed; skipping rg cancellation test")
	}

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	script := filepath.Join(dir, "fake-rg")
	source := "#!/bin/sh\ntrap 'exit 0' TERM\n: > " + strconv.Quote(ready) + "\nwhile :; do :; done\n"
	require.NoError(t, os.WriteFile(script, []byte(source), 0o755))

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() {
		_, err := rgGrepEngine{rgPath: script}.search(ctx, grepInput{
			workspaceRoot: dir,
			absPath:       dir,
			pattern:       "hit",
			outputMode:    outputModeContent,
			headLimit:     defaultGrepHeadLimit,
			re:            regexp.MustCompile("hit"),
		})
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 2*time.Second, 5*time.Millisecond, "fake rg did not start")
	cancel()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("rg engine did not return after context cancellation")
	}
}
