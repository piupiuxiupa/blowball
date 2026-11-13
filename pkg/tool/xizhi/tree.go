package xizhi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	defaultTreeDepth = 3
	maxTreeDepth     = 10
)

// treeNode is one entry in the nested tree returned by xizhi_tree.
type treeNode struct {
	Name     string     `json:"name"`
	Type     string     `json:"type"` // "file" or "dir"
	Size     int64      `json:"size,omitempty"`
	Children []treeNode `json:"children,omitempty"`
}

// treeResult is the JSON-serializable result returned by Tree.
type treeResult struct {
	Path  string     `json:"path"`
	Depth int        `json:"depth"`
	Tree  []treeNode `json:"tree"`
}

// Tree returns a nested representation of the directory at relPath, recursing
// up to depth levels. A depth of zero or less is normalised to the default (3);
// values above the maximum (10) are clamped. Hidden entries are omitted unless
// includeHidden is true.
func Tree(workspaceRoot, relPath string, depth int, includeHidden bool) (any, error) {
	relPath = normalizePath(relPath)
	absPath, err := validatePath(workspaceRoot, relPath)
	if err != nil {
		return nil, err
	}

	if depth <= 0 {
		depth = defaultTreeDepth
	}
	if depth > maxTreeDepth {
		depth = maxTreeDepth
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("directory not found: %w", err)
		}
		return nil, fmt.Errorf("xizhi tree: stat %q: %w", absPath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("xizhi tree: %q is not a directory", relPath)
	}

	nodes, err := buildTree(absPath, depth, includeHidden)
	if err != nil {
		return nil, err
	}
	return treeResult{Path: relPath, Depth: depth, Tree: nodes}, nil
}

func buildTree(dir string, depth int, includeHidden bool) ([]treeNode, error) {
	if depth == 0 {
		return nil, nil
	}

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("xizhi tree: read dir %q: %w", dir, err)
	}

	nodes := make([]treeNode, 0, len(dirEntries))
	for _, entry := range dirEntries {
		name := entry.Name()
		if !includeHidden && isHiddenName(name) {
			continue
		}

		node := treeNode{Name: name, Type: "file"}
		if entry.IsDir() {
			node.Type = "dir"
			children, err := buildTree(filepath.Join(dir, name), depth-1, includeHidden)
			if err != nil {
				return nil, err
			}
			node.Children = children
		} else {
			fi, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("xizhi tree: entry info %q: %w", name, err)
			}
			node.Size = fi.Size()
		}
		nodes = append(nodes, node)
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}
