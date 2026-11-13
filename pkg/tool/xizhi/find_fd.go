package xizhi

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// resolveFindEngine probes PATH for fd (then the Debian/Ubuntu fd-find binary
// name fdfind) and falls back to the Go engine when neither is installed.
func resolveFindEngine() findEngine {
	if p, err := exec.LookPath("fd"); err == nil {
		return fdFindEngine{fdPath: p}
	}
	if p, err := exec.LookPath("fdfind"); err == nil {
		return fdFindEngine{fdPath: p}
	}
	return goFindEngine{}
}

// defaultFindEngine resolves the find engine once per process (see
// resolveFindEngine). The lookup is cached so repeated turns do not re-probe.
// It mirrors defaultEngine in grep_engine.go.
var defaultFindEngine = sync.OnceValue(resolveFindEngine)

// fdFindEngine drives a system-installed fd binary, parsing its line output
// into the same findEntry shape the Go engine produces. The shared mapper
// (buildFindResult) then normalizes ordering, pagination and truncation, so
// the final findResult is identical to the Go fallback.
//
// Engine alignment with goFindEngine (chosen via fd flags, semantics verified
// against fd 10.4.2):
//   - --no-ignore always: the Go walk does not read .gitignore.
//   - -s --case-sensitive pins case-sensitivity (fd's default smart-case is
//     disabled); -i replaces it when ignore_case is set.
//   - --hidden iff includeHidden: the Go walk skips hidden entries unless
//     asked (an explicitly given hidden search root is still searched by both).
//   - -t f / -t d map the type filter; "any" passes BOTH so symlinks (which fd
//     classifies as neither file nor directory) are excluded — matching the
//     Go engine, which skips symlinks entirely. fd never follows symlinks.
//   - -d N bounds the depth: a path's depth is its component count below the
//     search root (immediate children are depth 1), as in the Go walk.
//   - fd exits 0 on success (no matches included); any non-zero exit is a real
//     error carrying stderr.
type fdFindEngine struct {
	fdPath string
}

func (e fdFindEngine) search(ctx context.Context, in findInput) (findEngineResult, error) {
	args := []string{"--no-ignore", "-s", "--case-sensitive"}
	if in.ignoreCase {
		args = append(args, "-i")
	}
	if in.includeHidden {
		args = append(args, "--hidden")
	}
	switch in.entryType {
	case findTypeFile:
		args = append(args, "-t", "f")
	case findTypeDirectory:
		args = append(args, "-t", "d")
	default: // any: both types, which also excludes symlinks (see above)
		args = append(args, "-t", "f", "-t", "d")
	}
	if in.maxDepth > 0 {
		args = append(args, "-d", strconv.Itoa(in.maxDepth))
	}
	// "--" keeps a pattern like "-note" from being parsed as a flag; the
	// pattern may be empty (fd's empty regex matches every name, like the Go
	// engine's nil regex) and "." is the search root, matching the Go walk.
	args = append(args, "--", in.pattern, ".")

	cmd := exec.CommandContext(ctx, e.fdPath, args...)
	cmd.Dir = in.absPath
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return findEngineResult{}, fmt.Errorf("xizhi_find: fd pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return findEngineResult{}, fmt.Errorf("xizhi_find: start fd: %w", err)
	}

	var er findEngineResult
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // tolerate very long paths
	for sc.Scan() {
		line := sc.Text()
		rel := filepath.ToSlash(strings.TrimPrefix(line, "./"))
		if rel == "" || rel == "." {
			continue
		}
		typ := findTypeFile
		if strings.HasSuffix(line, "/") {
			typ = findTypeDirectory
			rel = strings.TrimSuffix(rel, "/")
		}
		er.entries = append(er.entries, findEntry{Path: rel, Type: typ})
		if len(er.entries) >= maxFindCollect {
			er.collectCapped = true
			break
		}
	}
	// A scanner error (a single path exceeding the 1 MiB buffer) ends the read
	// early; the entries collected so far are still valid, so it is tolerated
	// as a partial result (mirroring the rg engine).
	_ = sc.Err()

	// If we stopped early, kill fd so it does not block on a full stdout pipe.
	if er.collectCapped {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	if waitErr != nil && !er.collectCapped {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			if ctx.Err() != nil {
				return findEngineResult{}, ctx.Err()
			}
			return findEngineResult{}, fmt.Errorf("xizhi_find: fd failed (exit %d): %s", ee.ExitCode(), strings.TrimSpace(stderr.String()))
		}
		if ctx.Err() != nil {
			return findEngineResult{}, ctx.Err()
		}
		return findEngineResult{}, fmt.Errorf("xizhi_find: fd wait: %w", waitErr)
	}

	return er, nil
}
