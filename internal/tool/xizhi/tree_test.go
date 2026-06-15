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

func TestTree_DefaultDepth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "c", "deep.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "top.txt"), []byte("y"), 0o644))

	res, err := Tree(root, ".", 0, false)
	require.NoError(t, err)
	got := res.(treeResult)
	assert.Equal(t, ".", got.Path)
	assert.Equal(t, defaultTreeDepth, got.Depth)

	// With default depth 3, the tree includes a -> b -> c with empty children.
	aNode := findTreeNode(got.Tree, "a")
	require.NotNil(t, aNode)
	bNode := findTreeNode(aNode.Children, "b")
	require.NotNil(t, bNode)
	cNode := findTreeNode(bNode.Children, "c")
	require.NotNil(t, cNode)
	assert.Empty(t, cNode.Children)
}

func TestTree_CustomDepth(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a", "b"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a", "b", "file.txt"), []byte("x"), 0o644))

	res, err := Tree(root, "a", 2, false)
	require.NoError(t, err)
	got := res.(treeResult)
	assert.Equal(t, "a", got.Path)
	assert.Equal(t, 2, got.Depth)

	bNode := findTreeNode(got.Tree, "b")
	require.NotNil(t, bNode)
	require.Len(t, bNode.Children, 1)
	assert.Equal(t, "file.txt", bNode.Children[0].Name)
	assert.Empty(t, bNode.Children[0].Children)
}

func TestTree_MaxDepthClamp(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "a"), 0o755))

	res, err := Tree(root, ".", 100, false)
	require.NoError(t, err)
	got := res.(treeResult)
	assert.Equal(t, maxTreeDepth, got.Depth)
}

func TestTree_HiddenExcluded(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".hidden"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "visible"), []byte("y"), 0o644))

	res, err := Tree(root, ".", 1, false)
	require.NoError(t, err)
	got := res.(treeResult)
	require.Len(t, got.Tree, 1)
	assert.Equal(t, "visible", got.Tree[0].Name)
}

func TestTree_PathOutsideWorkspace_Rejected(t *testing.T) {
	root := t.TempDir()
	_, err := Tree(root, "../../etc/passwd", 3, false)
	if !errors.Is(err, ErrPathOutsideWorkspace) {
		t.Fatalf("err = %v, want ErrPathOutsideWorkspace", err)
	}
}

func TestTree_ViaRegistry_Execute(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0o644))

	r := newTestRegistry(t)
	RegisterAll(r, root, testXizhiConfig())

	spec, ok := r.Get(NameTree)
	require.True(t, ok)

	args, err := json.Marshal(treeArgs{Path: "."})
	require.NoError(t, err)

	res, err := spec.Execute(t.Context(), args)
	require.NoError(t, err)

	b, err := json.Marshal(res)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"name":"file.txt"`)
}

func findTreeNode(nodes []treeNode, name string) *treeNode {
	for i := range nodes {
		if nodes[i].Name == name {
			return &nodes[i]
		}
	}
	return nil
}
