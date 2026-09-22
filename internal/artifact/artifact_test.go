package artifact

import (
	"context"
	"os"
	"path/filepath"
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

func TestDetect_FindsFreshRegularFiles(t *testing.T) {
	root := t.TempDir()
	since := time.Now().Add(-time.Minute)
	writeFile(t, root, "reports/a.docx", "aaa")
	writeFile(t, root, "notes.md", "hello")

	got, err := Detect(root, since)
	require.NoError(t, err)
	assert.Equal(t, []string{"notes.md", "reports/a.docx"}, got)
}

func TestDetect_ExcludesTmpHiddenAndStale(t *testing.T) {
	root := t.TempDir()
	recent := time.Now()
	since := recent.Add(-time.Minute)
	writeFile(t, root, "tmp/scratch.txt", "scratch")
	writeFile(t, root, ".blowball/skills/s/SKILL.md", "skill")
	writeFile(t, root, ".pip/x.py", "x")
	writeFile(t, root, "sub/.cache/y.txt", "y")
	writeFile(t, root, ".hiddenfile", "h")
	writeFile(t, root, "keep.txt", "keep")
	writeFile(t, root, "empty.txt", "")

	// Backdate one file beyond the window.
	old := filepath.Join(root, "old.txt")
	require.NoError(t, os.WriteFile(old, []byte("old"), 0o644))
	stale := recent.Add(-time.Hour)
	require.NoError(t, os.Chtimes(old, stale, stale))

	got, err := Detect(root, since)
	require.NoError(t, err)
	assert.Equal(t, []string{"keep.txt"}, got)
}

func newService(t *testing.T, idx Index) (*Service, *fakeStore) {
	t.Helper()
	store := &fakeStore{blobs: map[string][]byte{}}
	return NewService(store, idx, 0), store
}

func TestFinalizeTurn_SnapshotsNewArtifact(t *testing.T) {
	ws := t.TempDir()
	writeFile(t, ws, "reports/a.docx", "v1-content")
	idx := &fakeIndex{}
	svc, _ := newService(t, idx)

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, time.Now().Add(-time.Minute))
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
	since := time.Now().Add(-time.Minute)

	first, err := svc.FinalizeTurn(context.Background(), "u1", ws, since)
	require.NoError(t, err)
	require.Len(t, first, 1)

	// Same content, touched mtime: dedup reuses the version id.
	require.NoError(t, os.Chtimes(filepath.Join(ws, "a.md"), time.Now(), time.Now()))
	again, err := svc.FinalizeTurn(context.Background(), "u1", ws, since)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, OpUpdate, again[0].Op)
	assert.Equal(t, first[0].VersionID, again[0].VersionID)
	assert.Len(t, idx.recs, 1, "dedup must not insert a new index row")

	// Changed content: new version, old version still readable.
	writeFile(t, ws, "a.md", "v2")
	second, err := svc.FinalizeTurn(context.Background(), "u1", ws, since)
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

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, time.Now().Add(-time.Minute))
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

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, time.Now().Add(-time.Minute))
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
	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, time.Now().Add(-time.Minute))
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

	arts, err := svc.FinalizeTurn(context.Background(), "u1", ws, time.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.Len(t, arts, 1)
	assert.Equal(t, "a.md", arts[0].Path)
	assert.Empty(t, arts[0].VersionID, "no store configured: announce only")
}
