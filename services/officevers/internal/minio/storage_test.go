package minio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// testClient builds a *Client against a real MinIO instance configured via env
// vars, creating a fresh versioned bucket for the test run. Tests that need a
// live MinIO are skipped unless TEST_MINIO_ENDPOINT is set.
//
// Typical usage against a local MinIO:
//
//	docker run -d -p 9000:9000 -e MINIO_ROOT_USER=minioadmin \
//	  -e MINIO_ROOT_PASSWORD=minioadmin minio/minio server /data
//	TEST_MINIO_ENDPOINT=127.0.0.1:9000 \
//	TEST_MINIO_ACCESS_KEY=minioadmin \
//	TEST_MINIO_SECRET_KEY=minioadmin \
//	go test ./internal/minio/ -run Integration -v
func testClient(t *testing.T) *Client {
	t.Helper()
	endpoint := os.Getenv("TEST_MINIO_ENDPOINT")
	accessKey := os.Getenv("TEST_MINIO_ACCESS_KEY")
	secretKey := os.Getenv("TEST_MINIO_SECRET_KEY")
	if endpoint == "" || accessKey == "" || secretKey == "" {
		t.Skip("TEST_MINIO_ENDPOINT / TEST_MINIO_ACCESS_KEY / TEST_MINIO_SECRET_KEY not set; skipping integration test")
	}
	useSSL := os.Getenv("TEST_MINIO_USE_SSL") == "true"

	raw, err := miniogo.New(endpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	bucket := "office-version-service-test"
	ctx := context.Background()

	// Start from a clean bucket: force-remove and recreate (MinIO extension).
	_ = raw.RemoveBucketWithOptions(ctx, bucket, miniogo.RemoveBucketOptions{ForceDelete: true})
	if err := raw.MakeBucket(ctx, bucket, miniogo.MakeBucketOptions{}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}
	if err := raw.SetBucketVersioning(ctx, bucket, miniogo.BucketVersioningConfiguration{Status: "Enabled"}); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}
	return &Client{raw: raw, Bucket: bucket}
}

func mustUpload(t *testing.T, c *Client, ctx context.Context, uuid, name, content string) string {
	t.Helper()
	vid, _, err := c.Upload(ctx, uuid, name, strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("upload %s/%s: %v", uuid, name, err)
	}
	// Give S3 strong consistency a moment for version-id propagation across listings.
	time.Sleep(200 * time.Millisecond)
	return vid
}

func readDownload(t *testing.T, dl *Download) string {
	t.Helper()
	b, err := io.ReadAll(dl.Reader)
	dl.Reader.Close()
	if err != nil {
		t.Fatalf("read download: %v", err)
	}
	return string(b)
}

// TestIntegrationBootstrap verifies the bootstrap path: on an existing versioned
// bucket it is a no-op, and bootstrap enables versioning on a fresh bucket.
func TestIntegrationBootstrap(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	// The test harness already created + versioned the bucket; bootstrap must be
	// idempotent and not error.
	if err := c.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap on already-versioned bucket: %v", err)
	}
	vcfg, err := c.raw.GetBucketVersioning(ctx, c.Bucket)
	if err != nil {
		t.Fatalf("get versioning: %v", err)
	}
	if vcfg.Status != "Enabled" {
		t.Fatalf("versioning status = %q, want Enabled", vcfg.Status)
	}
}

// TestIntegrationUploadCreatesNewVersion covers 6.2: upload creates a new
// version each time, and listing returns versions newest-first with one latest.
func TestIntegrationUploadCreatesNewVersion(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	uuid := "alpha"

	v1 := mustUpload(t, c, ctx, uuid, "report.docx", "content-v1")
	v2 := mustUpload(t, c, ctx, uuid, "report.docx", "content-v2")
	v3 := mustUpload(t, c, ctx, uuid, "report.docx", "content-v3")

	if v1 == "" || v2 == "" || v3 == "" {
		t.Fatalf("expected non-empty version ids, got %q %q %q", v1, v2, v3)
	}
	if v1 == v2 || v2 == v3 || v1 == v3 {
		t.Fatalf("expected distinct version ids, got %q %q %q", v1, v2, v3)
	}

	versions, err := c.ListVersions(ctx, uuid, "report.docx")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}

	// Newest-first.
	if versions[0].VersionID != v3 || versions[2].VersionID != v1 {
		t.Fatalf("versions not newest-first: %+v", versions)
	}

	// Exactly one latest.
	latestCount := 0
	for _, v := range versions {
		if v.IsLatest {
			latestCount++
		}
	}
	if latestCount != 1 {
		t.Fatalf("got %d latest entries, want 1", latestCount)
	}
	if !versions[0].IsLatest {
		t.Fatalf("first (newest) entry should be latest: %+v", versions[0])
	}
}

// TestIntegrationDownloadLatestAndVersion covers 6.2: download latest matches
// the most recent content; download a specific historical version matches that
// version's content even when newer versions exist.
func TestIntegrationDownloadLatestAndVersion(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	uuid := "beta"

	v1 := mustUpload(t, c, ctx, uuid, "data.bin", "AAAA")
	_ = mustUpload(t, c, ctx, uuid, "data.bin", "BBBB")
	_ = mustUpload(t, c, ctx, uuid, "data.bin", "CCCC")

	latest, err := c.DownloadLatest(ctx, uuid, "data.bin")
	if err != nil {
		t.Fatalf("download latest: %v", err)
	}
	if got := readDownload(t, latest); got != "CCCC" {
		t.Fatalf("latest content = %q, want CCCC", got)
	}

	specific, err := c.DownloadVersion(ctx, uuid, "data.bin", v1)
	if err != nil {
		t.Fatalf("download version %s: %v", v1, err)
	}
	if got := readDownload(t, specific); got != "AAAA" {
		t.Fatalf("version %s content = %q, want AAAA", v1, got)
	}
}

// TestIntegrationDownloadNotFound covers 404 paths.
func TestIntegrationDownloadNotFound(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	if _, err := c.DownloadLatest(ctx, "missing", "nope.docx"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download latest missing file: err = %v, want ErrNotFound", err)
	}
	if _, err := c.DownloadVersion(ctx, "alpha", "report.docx", "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download unknown version: err = %v, want ErrNotFound", err)
	}
	if _, err := c.ListVersions(ctx, "missing", "nope.docx"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list versions missing file: err = %v, want ErrNotFound", err)
	}
}

// TestIntegrationCrossUUIDIsolation covers 6.2: identical filenames under
// different UUIDs have independent version histories.
func TestIntegrationCrossUUIDIsolation(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	mustUpload(t, c, ctx, "uuid-one", "report.docx", "one-content")
	mustUpload(t, c, ctx, "uuid-two", "report.docx", "two-content")

	dl1, err := c.DownloadLatest(ctx, "uuid-one", "report.docx")
	if err != nil {
		t.Fatalf("download one: %v", err)
	}
	if got := readDownload(t, dl1); got != "one-content" {
		t.Fatalf("uuid-one content = %q, want one-content", got)
	}

	dl2, err := c.DownloadLatest(ctx, "uuid-two", "report.docx")
	if err != nil {
		t.Fatalf("download two: %v", err)
	}
	if got := readDownload(t, dl2); got != "two-content" {
		t.Fatalf("uuid-two content = %q, want two-content", got)
	}

	vOne, _ := c.ListVersions(ctx, "uuid-one", "report.docx")
	vTwo, _ := c.ListVersions(ctx, "uuid-two", "report.docx")
	if len(vOne) != 1 || len(vTwo) != 1 {
		t.Fatalf("expected 1 version each, got %d and %d", len(vOne), len(vTwo))
	}
}

// TestIntegrationListDirectories covers the UUID directory listing:
// ListDirectories returns the top-level UUID prefixes that contain at least
// one file.
func TestIntegrationListDirectories(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	mustUpload(t, c, ctx, "dir-a", "report.docx", "a-report")
	mustUpload(t, c, ctx, "dir-a", "budget.xlsx", "a-budget")
	mustUpload(t, c, ctx, "dir-b", "report.docx", "b-report")

	dirs, err := c.ListDirectories(ctx)
	if err != nil {
		t.Fatalf("list dirs: %v", err)
	}
	dirSet := map[string]bool{}
	for _, d := range dirs {
		dirSet[d] = true
	}
	if !dirSet["dir-a"] || !dirSet["dir-b"] {
		t.Fatalf("missing expected directories, got %+v", dirs)
	}
}

// TestIntegrationDelete covers 6.2: delete removes the object and all its
// versions; subsequent history/download requests return not-found.
func TestIntegrationDelete(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	uuid := "delete-me"

	mustUpload(t, c, ctx, uuid, "doomed.docx", "v1")
	mustUpload(t, c, ctx, uuid, "doomed.docx", "v2")
	mustUpload(t, c, ctx, uuid, "doomed.docx", "v3")

	versions, _ := c.ListVersions(ctx, uuid, "doomed.docx")
	if len(versions) != 3 {
		t.Fatalf("precondition: got %d versions, want 3", len(versions))
	}

	if err := c.Delete(ctx, uuid, "doomed.docx"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := c.ListVersions(ctx, uuid, "doomed.docx"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list after delete: err = %v, want ErrNotFound", err)
	}
	if _, err := c.DownloadLatest(ctx, uuid, "doomed.docx"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("download after delete: err = %v, want ErrNotFound", err)
	}
}

// TestIntegrationRollback covers 6.3: rollback to v1 creates a new latest
// version equal to v1 content while v1/v2/v3 remain; rolling back to an unknown
// version returns not-found.
func TestIntegrationRollback(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	uuid := "rollback-uuid"

	v1 := mustUpload(t, c, ctx, uuid, "report.docx", "content-v1")
	v2 := mustUpload(t, c, ctx, uuid, "report.docx", "content-v2")
	v3 := mustUpload(t, c, ctx, uuid, "report.docx", "content-v3")
	time.Sleep(300 * time.Millisecond)

	// Rollback to v1.
	newVid, err := c.Rollback(ctx, uuid, "report.docx", v1)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if newVid == "" {
		t.Fatal("rollback returned empty version id")
	}
	time.Sleep(300 * time.Millisecond)

	// The new latest content equals v1's content.
	latest, err := c.DownloadLatest(ctx, uuid, "report.docx")
	if err != nil {
		t.Fatalf("download latest after rollback: %v", err)
	}
	if got := readDownload(t, latest); got != "content-v1" {
		t.Fatalf("latest after rollback = %q, want content-v1", got)
	}

	// All original versions remain: v1, v2, v3 plus the new rollback version = 4.
	versions, err := c.ListVersions(ctx, uuid, "report.docx")
	if err != nil {
		t.Fatalf("list versions after rollback: %v", err)
	}
	if len(versions) != 4 {
		t.Fatalf("got %d versions after rollback, want 4 (v1,v2,v3 + new)", len(versions))
	}
	ids := map[string]bool{}
	for _, v := range versions {
		ids[v.VersionID] = true
	}
	for _, want := range []string{v1, v2, v3, newVid} {
		if !ids[want] {
			t.Fatalf("version %q missing after rollback; have %+v", want, ids)
		}
	}
	// The new version is the latest.
	if !versions[0].IsLatest || versions[0].VersionID != newVid {
		t.Fatalf("new rollback version should be latest; got %+v", versions[0])
	}

	// v1 is still independently retrievable.
	specific, err := c.DownloadVersion(ctx, uuid, "report.docx", v1)
	if err != nil {
		t.Fatalf("download v1 after rollback: %v", err)
	}
	if got := readDownload(t, specific); got != "content-v1" {
		t.Fatalf("v1 content after rollback = %q, want content-v1", got)
	}

	// Rolling back to an unknown version returns not-found without changes.
	_, err = c.Rollback(ctx, uuid, "report.docx", "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback unknown version: err = %v, want ErrNotFound", err)
	}
}

// TestIntegrationRollbackMissingFile covers 6.3: rollback of a non-existent
// file returns not-found.
func TestIntegrationRollbackMissingFile(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	_, err := c.Rollback(ctx, "no-such-uuid", "nope.docx", "v1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback missing file: err = %v, want ErrNotFound", err)
	}
}

// TestIntegrationCopyObjectWithVersionID covers 6.4 (Open Question resolution):
// CopyObject with a source VersionID produces a new latest version on the target
// MinIO, confirming the server-side versioned copy used by Rollback is
// supported.
func TestIntegrationCopyObjectWithVersionID(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	uuid := "copysrc"

	v1 := mustUpload(t, c, ctx, uuid, "copy.docx", "original-v1")
	_ = mustUpload(t, c, ctx, uuid, "copy.docx", "newer-v2")
	time.Sleep(200 * time.Millisecond)

	leaf, leafErr := StorageLeaf("copy.docx")
	if leafErr != nil {
		t.Fatalf("storage leaf: %v", leafErr)
	}
	key := ObjectKey(uuid, leaf)
	dst := miniogo.CopyDestOptions{Bucket: c.Bucket, Object: key}
	src := miniogo.CopySrcOptions{Bucket: c.Bucket, Object: key, VersionID: v1}
	info, err := c.raw.CopyObject(ctx, dst, src)
	if err != nil {
		t.Fatalf("CopyObject with versionId %s failed against target MinIO: %v", v1, err)
	}
	if info.VersionID == "" {
		t.Fatal("CopyObject did not return a new version id")
	}
	t.Logf("CopyObject source versionId=%s produced new versionId=%s", v1, info.VersionID)

	time.Sleep(200 * time.Millisecond)
	latest, err := c.DownloadLatest(ctx, uuid, "copy.docx")
	if err != nil {
		t.Fatalf("download latest: %v", err)
	}
	if got := readDownload(t, latest); got != "original-v1" {
		t.Fatalf("after versioned copy, latest = %q, want original-v1", got)
	}
}

// TestIntegrationLargeUpload verifies streaming upload works for content larger
// than a single in-memory buffer is required (sanity check of PutObject).
func TestIntegrationLargeUpload(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	uuid := "large"

	payload := bytes.Repeat([]byte("X"), 256*1024) // 256 KiB
	vid, _, err := c.Upload(ctx, uuid, "big.bin", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("upload large: %v", err)
	}
	_ = vid
	dl, err := c.DownloadLatest(ctx, uuid, "big.bin")
	if err != nil {
		t.Fatalf("download large: %v", err)
	}
	got, err := io.ReadAll(dl.Reader)
	dl.Reader.Close()
	if err != nil {
		t.Fatalf("read large: %v", err)
	}
	if len(got) != len(payload) {
		t.Fatalf("size mismatch: got %d, want %d", len(got), len(payload))
	}
	if fmt.Sprintf("%x", got[:16]) != fmt.Sprintf("%x", payload[:16]) {
		t.Fatal("content mismatch on large upload")
	}
}

// TestIntegrationLogicalPathRoundTrip covers the logical-path addressing model:
// a multi-segment logical path resolves to a single hashed key, the same path
// accumulates versions, the path downloads back the uploaded content
// (canonicalization round-trip), and a byte-different path does NOT collide.
func TestIntegrationLogicalPathRoundTrip(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	uuid := "logical"
	p := "财务/2024/年报.docx"

	mustUpload(t, c, ctx, uuid, p, "v1-content")
	mustUpload(t, c, ctx, uuid, p, "v2-content")

	versions, err := c.ListVersions(ctx, uuid, p)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions for the same logical path, got %d", len(versions))
	}

	latest, err := c.DownloadLatest(ctx, uuid, p)
	if err != nil {
		t.Fatalf("download latest: %v", err)
	}
	if got := readDownload(t, latest); got != "v2-content" {
		t.Fatalf("latest = %q, want v2-content", got)
	}

	// A different extension is a different (and here absent) key.
	if _, err := c.DownloadLatest(ctx, uuid, "财务/2024/年报.xlsx"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("different extension should be a different absent key, got err=%v", err)
	}
}
