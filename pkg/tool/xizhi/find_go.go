package xizhi

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// goFindEngine is the pure-Go fallback used when fd is not installed. It
// walks the search root and matches each entry's base name against the
// compiled regex. It produces the same findEntry shape as the fd engine; the
// shared mapper in find.go normalizes ordering, pagination and truncation so
// both engines yield an identical findResult.
type goFindEngine struct{}

func (goFindEngine) search(ctx context.Context, in findInput) (findEngineResult, error) {
	var er findEngineResult
	walkErr := filepath.WalkDir(in.absPath, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// Skip unreadable entries rather than aborting the whole search.
			return nil
		}
		// The search root itself is never listed as a match (fd semantics).
		if p == in.absPath {
			return nil
		}
		// Do not follow or list symlinks (mirrors goGrepEngine; the fd engine
		// excludes them via its type filters).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		isDir := d.IsDir()
		if !in.includeHidden && isHiddenName(d.Name()) {
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(in.absPath, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if in.maxDepth > 0 && strings.Count(rel, "/")+1 > in.maxDepth {
			// Everything below an over-deep directory is deeper still; prune it.
			if isDir {
				return filepath.SkipDir
			}
			return nil
		}
		// A directory excluded by the type filter may still contain matches,
		// so it is skipped from the results but not pruned from the walk.
		if in.entryType == findTypeFile && isDir {
			return nil
		}
		if in.entryType == findTypeDirectory && !isDir {
			return nil
		}
		if in.re != nil && !in.re.MatchString(d.Name()) {
			return nil
		}
		typ := findTypeFile
		if isDir {
			typ = findTypeDirectory
		}
		er.entries = append(er.entries, findEntry{Path: rel, Type: typ})
		if len(er.entries) >= maxFindCollect {
			er.collectCapped = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		if ctx.Err() != nil {
			return findEngineResult{}, ctx.Err()
		}
		return findEngineResult{}, fmt.Errorf("xizhi_find: walk %q: %w", in.absPath, walkErr)
	}
	return er, nil
}
