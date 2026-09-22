package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// VersionStore is the content side of the version store: Put snapshots bytes
// and returns the store-assigned version id; Get reads one version back.
// Metadata (ownership, dedup, as-of) lives in the MySQL Index — the store
// knows nothing about it.
type VersionStore interface {
	Put(ctx context.Context, userID, path string, data []byte) (versionID string, err error)
	Get(ctx context.Context, userID, path, versionID string) ([]byte, error)
}

// OfficeVersStore is a VersionStore backed by the external office-vers
// service (S3 object versioning; see office-vers docs/openapi.yaml). Writes
// are POST /documents/{uuid}/{path} with the raw bytes; office-vers derives
// the stored content type from the path extension and assigns a MinIO version
// id. The service is unauthenticated by design — it must stay on the internal
// network, and blowball never hands its URLs to the frontend (reads are
// proxied through the authenticated backend).
type OfficeVersStore struct {
	base string
	http *http.Client
}

// NewOfficeVersStore builds a client for the office-vers base URL (the
// onlyoffice.version_service_url config value). A 10s default keeps turn-end
// snapshotting from hanging on a sick service.
func NewOfficeVersStore(base string) *OfficeVersStore {
	return &OfficeVersStore{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 10 * time.Second}}
}

// escapePath percent-escapes each slash-separated segment, preserving the
// separators (same rule the workspace OnlyOffice config builder uses: a raw
// "+" or "%" in a filename must survive the trip).
func escapePath(rel string) string {
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// Put uploads data as a new version of {userID}/{path} and returns the
// office-vers-assigned version id.
func (s *OfficeVersStore) Put(ctx context.Context, userID, path string, data []byte) (string, error) {
	u := s.base + "/documents/" + url.PathEscape(userID) + "/" + escapePath(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("office-vers put %q: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("office-vers put %q: status %d", path, resp.StatusCode)
	}
	var out struct {
		VersionID string `json:"versionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.VersionID == "" {
		return "", fmt.Errorf("office-vers put %q: bad response (err=%v)", path, err)
	}
	return out.VersionID, nil
}

// Get downloads one version's bytes.
func (s *OfficeVersStore) Get(ctx context.Context, userID, path, versionID string) ([]byte, error) {
	u := s.base + "/documents/" + url.PathEscape(userID) + "/" + escapePath(path) +
		"?action=version&versionId=" + url.QueryEscape(versionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("office-vers get %q@%s: %w", path, versionID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("office-vers get %q@%s: status %d", path, versionID, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
