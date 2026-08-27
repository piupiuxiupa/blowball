package luban

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool/skill"
)

// Tree-depth bounds mirroring xizhi_tree: default 3, clamped to a maximum of 10
// so a deeply nested skill cannot blow up the result.
const (
	defaultSkillTreeDepth = 3
	maxSkillTreeDepth     = 10
)

// skillFileEntry is one direct child returned by luban_list_skill_files. It
// mirrors xizhi_list_files' listEntry: size is meaningful only for files.
type skillFileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"`
}

// listSkillFilesResult is the JSON shape returned by ListSkillFiles.
type listSkillFilesResult struct {
	Path    string           `json:"path"`
	Entries []skillFileEntry `json:"entries"`
}

// skillTreeNode is one entry in the nested tree returned by luban_tree_skill,
// mirroring xizhi_tree's treeNode: directory nodes carry children; size is
// omitted for directories.
type skillTreeNode struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"` // "file" or "dir"
	Size     int64           `json:"size,omitempty"`
	Children []skillTreeNode `json:"children,omitempty"`
}

// treeSkillResult is the JSON shape returned by TreeSkill.
type treeSkillResult struct {
	Path  string          `json:"path"`
	Depth int             `json:"depth"`
	Tree  []skillTreeNode `json:"tree"`
}

// hiddenName reports whether a file or directory name should be considered
// hidden (starts with "."), mirroring xizhi's isHiddenName. Hidden entries are
// excluded unless includeHidden is true, so a git-cloned skill's .git directory
// is hidden by default.
func hiddenName(name string) bool {
	return name != "" && name[0] == '.'
}

// resolveSkillSubdir resolves name to its skill directory (user skills override
// global skills of the same name; after a local miss the skill market
// allowlist resolves the name to {data-dir}/skills-market{path} — user >
// global > market, identical precedence to luban_read_skill), then confines
// optional subPath within it using the same rules as skill.ReadPath (absolute
// paths, ".." escapes, and symlinks resolving outside the skill directory are
// rejected — market skill directories get the identical confinement). It
// returns the absolute target directory, the skill root, and a display path
// (subPath, or "." when empty/omitted). toolName prefixes the returned errors
// (e.g. "luban_list_skill_files"). A disk-sync-lagged market directory passes
// resolution here and the caller's statSkillDir surfaces "directory not found"
// (the API is the visibility truth; sync lag is a use-point error).
func resolveSkillSubdir(ctx context.Context, loader *skill.Loader, market *skillmarket.Client, name, subPath, userID, toolName string) (target, skillRoot, display string, err error) {
	if verr := validateSkillName(name); verr != nil {
		return "", "", "", fmt.Errorf("%s: %w", toolName, verr)
	}
	root, ok := loader.SkillDir(name, userID)
	if !ok {
		// Market fallback (third source); nil client = capability off = miss.
		root, ok = marketSkillDir(ctx, market, name, userID)
	}
	if !ok {
		return "", "", "", fmt.Errorf("%s: skill %q not found", toolName, name)
	}
	if strings.TrimSpace(subPath) == "" {
		return root, root, ".", nil
	}
	abs, verr := skill.ValidateSubPath(root, subPath)
	if verr != nil {
		return "", "", "", fmt.Errorf("%s: %w", toolName, verr)
	}
	return abs, root, subPath, nil
}

// statSkillDir stats target and confirms it is a directory, returning errors
// prefixed with toolName. ValidateSubPath returns a non-resolved joined path
// when the target does not exist, so the subsequent Stat surfaces a clean
// "directory not found".
func statSkillDir(target, toolName string) (os.FileInfo, error) {
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: directory not found", toolName)
		}
		return nil, fmt.Errorf("%s: stat: %w", toolName, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", toolName)
	}
	return info, nil
}

// ListSkillFiles lists the immediate children of the named skill's directory
// (or a sub-directory of it when subPath is provided), one level only. It
// mirrors xizhi_list_files' shape. Hidden entries are omitted unless
// includeHidden is true. Name resolution is user > global > market (a nil
// market client keeps it purely local).
func ListSkillFiles(ctx context.Context, loader *skill.Loader, market *skillmarket.Client, name, subPath, userID string, includeHidden bool) (any, error) {
	const toolName = "luban_list_skill_files"
	target, _, display, err := resolveSkillSubdir(ctx, loader, market, name, subPath, userID, toolName)
	if err != nil {
		return nil, err
	}
	if _, err := statSkillDir(target, toolName); err != nil {
		return nil, err
	}

	dirEntries, err := os.ReadDir(target)
	if err != nil {
		return nil, fmt.Errorf("%s: read dir: %w", toolName, err)
	}

	entries := make([]skillFileEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		name := entry.Name()
		if !includeHidden && hiddenName(name) {
			continue
		}
		var typ string
		var size int64
		if entry.IsDir() {
			typ = "dir"
		} else {
			typ = "file"
			fi, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("%s: entry info %q: %w", toolName, name, err)
			}
			size = fi.Size()
		}
		entries = append(entries, skillFileEntry{Name: name, Type: typ, Size: size})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return listSkillFilesResult{Path: display, Entries: entries}, nil
}

// TreeSkill returns a nested representation of the named skill's directory (or
// a sub-directory of it when subPath is provided), recursing up to depth levels.
// It mirrors xizhi_tree's shape. depth defaults to 3 and is clamped to a
// maximum of 10. Hidden entries are omitted unless includeHidden is true. Name
// resolution is user > global > market (a nil market client keeps it purely
// local).
func TreeSkill(ctx context.Context, loader *skill.Loader, market *skillmarket.Client, name, subPath, userID string, depth int, includeHidden bool) (any, error) {
	const toolName = "luban_tree_skill"
	target, _, display, err := resolveSkillSubdir(ctx, loader, market, name, subPath, userID, toolName)
	if err != nil {
		return nil, err
	}
	if _, err := statSkillDir(target, toolName); err != nil {
		return nil, err
	}

	if depth <= 0 {
		depth = defaultSkillTreeDepth
	}
	if depth > maxSkillTreeDepth {
		depth = maxSkillTreeDepth
	}

	nodes, err := buildSkillTree(target, depth, includeHidden, toolName)
	if err != nil {
		return nil, err
	}
	return treeSkillResult{Path: display, Depth: depth, Tree: nodes}, nil
}

// buildSkillTree recursively builds the nested node list for dir, recursing up
// to depth levels. Mirrors xizhi's buildTree, including the hidden-name and
// sort semantics.
func buildSkillTree(dir string, depth int, includeHidden bool, toolName string) ([]skillTreeNode, error) {
	if depth == 0 {
		return nil, nil
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: read dir: %w", toolName, err)
	}
	nodes := make([]skillTreeNode, 0, len(dirEntries))
	for _, entry := range dirEntries {
		name := entry.Name()
		if !includeHidden && hiddenName(name) {
			continue
		}
		node := skillTreeNode{Name: name, Type: "file"}
		if entry.IsDir() {
			node.Type = "dir"
			children, err := buildSkillTree(filepath.Join(dir, name), depth-1, includeHidden, toolName)
			if err != nil {
				return nil, err
			}
			node.Children = children
		} else {
			fi, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("%s: entry info %q: %w", toolName, name, err)
			}
			node.Size = fi.Size()
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}
