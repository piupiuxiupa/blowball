package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lush/blowball/internal/model"
)

// fakeStore is an in-memory VersionStore: each Put mints a sequential
// version id and keeps the bytes.
type fakeStore struct {
	blobs map[string][]byte
	n     int
	err   error
}

func (f *fakeStore) Put(_ context.Context, userID, path string, data []byte) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.n++
	vid := "v-" + string(rune('0'+f.n))
	f.blobs[userID+"/"+path+"/"+vid] = data
	return vid, nil
}

func (f *fakeStore) Get(_ context.Context, userID, path, versionID string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.blobs[userID+"/"+path+"/"+versionID], nil
}

// fakeIndex is an in-memory artifact.Index for service tests.
type fakeIndex struct {
	recs []model.FileVersion
	err  error
}

func (f *fakeIndex) InsertVersion(_ context.Context, rec model.FileVersion) error {
	if f.err != nil {
		return f.err
	}
	rec.CreateTime = time.Now()
	f.recs = append(f.recs, rec)
	return nil
}

func (f *fakeIndex) LatestVersion(_ context.Context, userID, path string) (*model.FileVersion, error) {
	return f.latest(userID, path, time.Time{})
}

func (f *fakeIndex) VersionAsOf(_ context.Context, userID, path string, before time.Time) (*model.FileVersion, error) {
	return f.latest(userID, path, before)
}

func (f *fakeIndex) latest(userID, path string, before time.Time) (*model.FileVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	var best *model.FileVersion
	for i := range f.recs {
		r := &f.recs[i]
		if r.UserID != userID || r.Path != path {
			continue
		}
		if !before.IsZero() && r.CreateTime.After(before) {
			continue
		}
		if best == nil || r.CreateTime.After(best.CreateTime) {
			best = r
		}
	}
	return best, nil
}

func (f *fakeIndex) VersionByID(_ context.Context, versionID string) (*model.FileVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.recs {
		if f.recs[i].VersionID == versionID {
			return &f.recs[i], nil
		}
	}
	return nil, nil
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o644))
}

func linkTo(rel string) string {
	return fmt.Sprintf("[file](blowball://workspace/%s)", rel)
}

func TestDeclaredPaths_ExtractsDecodesDedupesAndSorts(t *testing.T) {
	content := strings.Join([]string{
		linkTo("reports/z%20b.md"),
		linkTo("reports/a.docx"),
		linkTo("reports/%E4%B8%AD%E6%96%87.docx"),
		linkTo("reports/a.docx"),
	}, "\n")

	got := declaredPaths(context.Background(), "u1", content)
	assert.Equal(t, []string{
		"reports/a.docx",
		"reports/z b.md",
		"reports/中文.docx",
	}, got)
}

func TestDeclaredPaths_RejectsInvalidTargets(t *testing.T) {
	content := strings.Join([]string{
		linkTo("tmp/scratch.txt"),
		linkTo(".blowball/state.json"),
		linkTo("sub/.cache/y.txt"),
		linkTo("../outside.txt"),
		linkTo("a/%2e%2e/outside.txt"),
		linkTo("//etc/passwd"),
		linkTo("a.md?version=1"),
		linkTo("a.md#fragment"),
		linkTo("a%zz.md"),
		"bare blowball://workspace/a.md is not a markdown artifact link",
	}, "\n")

	got := declaredPaths(context.Background(), "u1", content)
	assert.Empty(t, got)
}

func newService(t *testing.T, idx Index) (*Service, *fakeStore) {
	t.Helper()
	store := &fakeStore{blobs: map[string][]byte{}}
	return NewService(store, idx, 0), store
}

func TestFinalizeTurn_SnapshotsNewArtifact(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "reports/a.docx", "v1-content")
	writeFile(t, ws, "reports/unlinked.docx", "not declared")
	idx := &fakeIndex{}
	svc, _ := newService(t, idx)

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, linkTo("reports/a.docx"))
	require.NoError(t, err)
	require.Len(t, arts, 1)
	a := arts[0]
	assert.Equal(t, "reports/a.docx", a.Path)
	assert.Equal(t, OpCreate, a.Op)
	assert.NotEmpty(t, a.VersionID)
	assert.Equal(t, int64(len("v1-content")), a.Size)

	// Blob readable back through OpenVersion.
	rec, data, err := svc.OpenVersion(context.Background(), "u1", a.VersionID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "v1-content", string(data))
}

func TestFinalizeTurn_OverwriteMintsNewVersion(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.md", "v1")
	idx := &fakeIndex{}
	svc, _ := newService(t, idx)
	content := linkTo("a.md")

	first, err := svc.FinalizeTurn(context.Background(), "u1", ws, content)
	require.NoError(t, err)
	require.Len(t, first, 1)

	// Same content linked again: dedup reuses the version id.
	again, err := svc.FinalizeTurn(context.Background(), "u1", ws, content)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, OpUpdate, again[0].Op)
	assert.Equal(t, first[0].VersionID, again[0].VersionID)
	assert.Len(t, idx.recs, 1, "dedup must not insert a new index row")

	// Changed content: new version, old version still readable.
	writeFile(t, ws, "a.md", "v2")
	second, err := svc.FinalizeTurn(context.Background(), "u1", ws, content)
	require.NoError(t, err)
	assert.NotEqual(t, first[0].VersionID, second[0].VersionID)
	_, oldData, err := svc.OpenVersion(context.Background(), "u1", first[0].VersionID)
	require.NoError(t, err)
	assert.Equal(t, "v1", string(oldData))
}

func TestFinalizeTurn_OverCapSkipsSnapshotButAnnounces(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "big.bin", "0123456789")
	idx := &fakeIndex{}
	svc := NewService(&fakeStore{blobs: map[string][]byte{}}, idx, 4) // cap 4 bytes

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, linkTo("big.bin"))
	require.NoError(t, err)
	require.Len(t, arts, 1)
	assert.Empty(t, arts[0].VersionID, "over-cap artifact keeps no version")
	assert.Empty(t, idx.recs)
}

func TestFinalizeTurn_IndexFailureDegrades(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.md", "v1")
	idx := &fakeIndex{err: assert.AnError}
	svc, _ := newService(t, idx)

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, linkTo("a.md"))
	require.NoError(t, err)
	require.Len(t, arts, 1)
	assert.Empty(t, arts[0].VersionID)
}

func TestResolve_LatestAndAsOf(t *testing.T) {
	idx := &fakeIndex{}
	svc, _ := newService(t, idx)
	t1 := time.Now().Add(-time.Hour)
	t2 := time.Now()
	idx.recs = []model.FileVersion{
		{UserID: "u1", Path: "a.md", VersionID: "v-old", CreateTime: t1},
		{UserID: "u1", Path: "a.md", VersionID: "v-new", CreateTime: t2},
	}

	rec, err := svc.Resolve(context.Background(), "u1", "a.md", time.Time{})
	require.NoError(t, err)
	assert.Equal(t, "v-new", rec.VersionID)

	rec, err = svc.Resolve(context.Background(), "u1", "a.md", t1.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "v-old", rec.VersionID)

	rec, err = svc.Resolve(context.Background(), "u1", "never.md", time.Time{})
	require.NoError(t, err)
	assert.Nil(t, rec)
}

func TestOpenVersion_CrossUserDenied(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.md", "v1")
	idx := &fakeIndex{}
	svc, _ := newService(t, idx)
	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, linkTo("a.md"))
	require.NoError(t, err)
	require.Len(t, arts, 1)

	rec, data, err := svc.OpenVersion(context.Background(), "u2", arts[0].VersionID)
	require.NoError(t, err)
	assert.Nil(t, rec)
	assert.Nil(t, data)
}

func TestFinalizeTurn_NilStoreAnnouncesWithoutVersion(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "a.md", "v1")
	svc := NewService(nil, &fakeIndex{}, 0)

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, linkTo("a.md"))
	require.NoError(t, err)
	require.Len(t, arts, 1)
	assert.Equal(t, "a.md", arts[0].Path)
	assert.Empty(t, arts[0].VersionID, "no store configured: announce only")
}

func TestFinalizeTurn_UnusableTargetsSkipped(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "target.txt", "regular")
	require.NoError(t, os.Mkdir(filepath.Join(ws, "directory"), 0o755))
	writeFile(t, ws, "empty.txt", "")
	require.NoError(t, os.Symlink(filepath.Join(ws, "target.txt"), filepath.Join(ws, "symlink.txt")))

	content := strings.Join([]string{
		linkTo("missing.txt"),
		linkTo("directory"),
		linkTo("empty.txt"),
		linkTo("symlink.txt"),
	}, "\n")

	svc, _ := newService(t, &fakeIndex{})
	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, content)
	require.NoError(t, err)
	assert.Empty(t, arts)
}

func TestFinalizeTurn_ConcurrentSessionsOnlySeeOwnLinks(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "reports/a.docx", "session a")
	writeFile(t, ws, "reports/b.docx", "session b")
	idx := &fakeIndex{}
	svc, _ := newService(t, idx)
	ctx := context.Background()

	a, err := svc.FinalizeTurn(ctx, "u1", ws, linkTo("reports/a.docx"))
	require.NoError(t, err)
	b, err := svc.FinalizeTurn(ctx, "u1", ws, linkTo("reports/b.docx"))
	require.NoError(t, err)

	require.Len(t, a, 1)
	assert.Equal(t, "reports/a.docx", a[0].Path)
	require.Len(t, b, 1)
	assert.Equal(t, "reports/b.docx", b[0].Path)
}
