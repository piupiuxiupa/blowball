package xizhi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// rgGrepEngine drives a system-installed ripgrep binary, parsing its --json
// (JSON Lines) output into the same rawMatch shape the Go engine produces. The
// shared mapper (buildGrepResult) then normalizes ordering, pagination and
// truncation, so the final grepResult is identical to the Go fallback.
//
// Engine alignment with goGrepEngine (chosen via rg flags):
//   - --no-ignore always: Go does not read .gitignore.
//   - --hidden iff includeHidden: Go skips hidden entries unless asked.
//   - rg skips binary files and does not follow symlinks by default, matching
//     the Go engine's NUL-byte and symlink handling.
//   - glob is filtered post-hoc by doublestar.Match against the file's base
//     name, identical to the Go walk (so the two engines agree on glob
//     semantics; rg's own --glob is not used).
//
// Context lines in content mode are sliced from the file via the shared
// contextAround helper (the single source of truth), guaranteeing parity even
// when an adjacent line is itself a match (rg would otherwise dedupe it).
type rgGrepEngine struct {
	rgPath string
}

// rgEvent is one line of rg --json output: a discriminated object keyed by type.
type rgEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// rgMatchData is the data payload of an rg event of type "match".
type rgMatchData struct {
	Path struct {
		Text string `json:"text"`
	} `json:"path"`
	Lines struct {
		Text string `json:"text"`
	} `json:"lines"`
	LineNumber int `json:"line_number"`
}

// rawLocation is a raw match location collected from rg before context is
// attached (context is sliced from the file later via the shared helper).
type rawLocation struct {
	file string
	line int
	text string
}

func (e rgGrepEngine) search(ctx context.Context, in grepInput) (engineResult, error) {
	// Build args linearly: output format, alignment flags, the pattern (via -e
	// so patterns starting with '-' are not mistaken for flags), then the path.
	args := []string{"--json", "--no-ignore"}
	if in.includeHidden {
		args = append(args, "--hidden")
	}
	if in.ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", in.pattern, ".")

	cmd := exec.CommandContext(ctx, e.rgPath, args...)
	cmd.Dir = in.absPath
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return engineResult{}, fmt.Errorf("xizhi grep: rg pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return engineResult{}, fmt.Errorf("xizhi grep: start rg: %w", err)
	}

	var locs []rawLocation
	capped := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20) // tolerate long matched lines
	for sc.Scan() {
		var ev rgEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Type != "match" {
			continue
		}
		var d rgMatchData
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			continue
		}
		file := filepath.ToSlash(strings.TrimPrefix(d.Path.Text, "./"))
		if in.glob != "" {
			if ok, _ := doublestar.Match(in.glob, filepath.Base(file)); !ok {
				continue
			}
		}
		locs = append(locs, rawLocation{file: file, line: d.LineNumber, text: strings.TrimRight(d.Lines.Text, "\r\n")})
		if len(locs) >= maxGrepCollect {
			capped = true
			break
		}
	}
	// A scanner error (e.g. a single JSON line exceeding the 4 MiB buffer on a
	// pathologically long matched line) ends the read early; the locations
	// collected so far are still valid, so we tolerate it as a partial result.
	_ = sc.Err()

	// If we stopped early, kill rg so it does not block on a full stdout pipe.
	if capped {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	if waitErr != nil && !capped {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			// rg exits 1 when no matches were found — a normal empty result,
			// not a failure. Any other code is a real error.
			if code := ee.ExitCode(); code != 1 {
				if ctx.Err() != nil {
					return engineResult{}, ctx.Err()
				}
				return engineResult{}, fmt.Errorf("xizhi grep: rg failed (exit %d): %s", code, strings.TrimSpace(stderr.String()))
			}
		} else {
			if ctx.Err() != nil {
				return engineResult{}, ctx.Err()
			}
			return engineResult{}, fmt.Errorf("xizhi grep: rg wait: %w", waitErr)
		}
	}

	return engineResult{matches: attachContext(in, locs), collectCapped: capped}, nil
}

// attachContext builds rawMatches from the collected locations. In content mode
// with context requested, it reads each matching file once and slices context
// via the shared contextAround helper (matching the Go engine exactly).
func attachContext(in grepInput, locs []rawLocation) []rawMatch {
	needContext := in.outputMode == outputModeContent && (in.contextBefore > 0 || in.contextAfter > 0)
	if !needContext {
		out := make([]rawMatch, len(locs))
		for i, l := range locs {
			out[i] = rawMatch{file: l.file, lineNumber: l.line, line: l.text}
		}
		return out
	}

	byFile := make(map[string][]rawLocation, len(locs))
	for _, l := range locs {
		byFile[l.file] = append(byFile[l.file], l)
	}

	var out []rawMatch
	for file, ls := range byFile {
		lines, ok := readTextLines(filepath.Join(in.absPath, filepath.FromSlash(file)))
		if !ok {
			continue
		}
		for _, l := range ls {
			m := rawMatch{file: file, lineNumber: l.line, line: l.text}
			m.contextBefore, m.contextAfter = contextAround(lines, l.line, in.contextBefore, in.contextAfter)
			out = append(out, m)
		}
	}
	return out
}
