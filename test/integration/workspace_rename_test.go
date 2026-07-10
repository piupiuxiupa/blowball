package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWorkspaceRename_File_Success renames a file within the workspace.
func TestWorkspaceRename_File_Success(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	token := authToken(t, defaultUserID)
	ws := env.fsSvc.UserWorkspace(defaultUserID)
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "old.md"), []byte("content"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/old.md",
		strings.NewReader(`{"new_path":"new.md"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "old.md", resp.OldPath)
	assert.Equal(t, "new.md", resp.NewPath)

	_, err := os.Stat(filepath.Join(ws, "old.md"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(filepath.Join(ws, "new.md"))
	require.NoError(t, err)
	assert.Equal(t, "content", string(data))
}

// TestWorkspaceRename_DestinationExists_409 verifies no overwrite when the
// destination already exists.
func TestWorkspaceRename_DestinationExists_409(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	token := authToken(t, defaultUserID)
	ws := env.fsSvc.UserWorkspace(defaultUserID)
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "src.md"), []byte("src"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "dst.md"), []byte("dst"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/src.md",
		strings.NewReader(`{"new_path":"dst.md"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	var body struct {
		Error struct{ Code string `json:"code"` } `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ALREADY_EXISTS", body.Error.Code)

	srcData, err := os.ReadFile(filepath.Join(ws, "src.md"))
	require.NoError(t, err)
	assert.Equal(t, "src", string(srcData))
}

// TestWorkspaceRename_MoveToSubdirectory moves a file into an existing
// subdirectory.
func TestWorkspaceRename_MoveToSubdirectory(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	token := authToken(t, defaultUserID)
	ws := env.fsSvc.UserWorkspace(defaultUserID)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.md"), []byte("move me"), 0o644))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/workspace/files/a.md",
		strings.NewReader(`{"new_path":"subdir/b.md"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp struct {
		NewPath string `json:"new_path"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "subdir/b.md", resp.NewPath)

	_, err := os.Stat(filepath.Join(ws, "a.md"))
	require.ErrorIs(t, err, os.ErrNotExist)
	data, err := os.ReadFile(filepath.Join(ws, "subdir", "b.md"))
	require.NoError(t, err)
	assert.Equal(t, "move me", string(data))
}

// TestWorkspaceRename_PathOutsideWorkspace_403 verifies traversal attempts are
// rejected for either source or destination.
func TestWorkspaceRename_PathOutsideWorkspace_403(t *testing.T) {
	env := newTestEnv(t, newScriptedLLMClient())
	token := authToken(t, defaultUserID)
	ws := env.fsSvc.UserWorkspace(defaultUserID)
	require.NoError(t, os.MkdirAll(ws, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "a.md"), []byte("a"), 0o644))

	cases := []struct {
		name    string
		path    string
		newPath string
	}{
		{"source outside", "../../etc/passwd", "x.md"},
		{"destination outside", "a.md", "../../etc/passwd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut,
				"/api/v1/workspace/files/"+tc.path,
				strings.NewReader(`{"new_path":"`+tc.newPath+`"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)

			require.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
			var body struct {
				Error struct{ Code string `json:"code"` } `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.Equal(t, "FORBIDDEN", body.Error.Code)
		})
	}
}
