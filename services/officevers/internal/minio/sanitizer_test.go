package minio

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
	"testing"
)

func TestValidateLogicalPath(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr error
	}{
		{"simple", "report.docx", nil},
		{"unicode", "报告.xlsx", nil},
		{"spaces", "my report.docx", nil},
		{"multi-segment", "财务/2024/年报.docx", nil},
		{"leading-dot leaf", ".gitignore", nil},
		{"deep", "a/b/c/d/e.docx", nil},

		{"empty", "", ErrEmptyPath},
		{"leading slash", "/report.docx", ErrInvalidPath},
		{"trailing slash", "dir/report.docx/", ErrInvalidPath},
		{"empty segment", "a//b.docx", ErrInvalidPath},
		{"dot segment", "a/./b.docx", ErrInvalidPath},
		{"dotdot segment", "a/../b.docx", ErrInvalidPath},
		{"lone dot", ".", ErrInvalidPath},
		{"lone dotdot", "..", ErrInvalidPath},
		{"backslash", "a\\b.docx", ErrInvalidPath},
		{"nul byte", "a\x00b.docx", ErrInvalidPath},
		{"control char", "a\x01b.docx", ErrInvalidPath},
		{"segment too long", "a/" + strings.Repeat("x", 256) + ".docx", ErrSegmentTooLong},
		{"path too long", strings.Repeat("a", maxPathLen+1), ErrSegmentTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateLogicalPath(tc.input)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateLogicalPath(%q) unexpected err: %v", tc.input, err)
				}
				return
			}
			if !errorIs(err, tc.wantErr) {
				t.Fatalf("ValidateLogicalPath(%q) err = %v, want %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestStorageLeaf(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"simple", "report.docx"},
		{"no extension", "report"},
		{"unicode", "报告.xlsx"},
		{"multi-segment", "财务/2024/年报.docx"},
		{"leading-dot leaf", ".gitignore"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf, err := StorageLeaf(tc.path)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			wantExt := path.Ext(tc.path)
			sum := sha256.Sum256([]byte(tc.path))
			want := hex.EncodeToString(sum[:]) + wantExt
			if leaf != want {
				t.Fatalf("leaf = %q, want %q", leaf, want)
			}
			// The hash part must be a 64-char lowercase hex digest.
			hashPart := strings.TrimSuffix(leaf, wantExt)
			if len(hashPart) != 64 {
				t.Fatalf("hash part len = %d, want 64: %q", len(hashPart), leaf)
			}
		})
	}
}

func TestStorageLeafRejectsInvalidPath(t *testing.T) {
	if _, err := StorageLeaf(""); !errorIs(err, ErrEmptyPath) {
		t.Fatalf("empty path err = %v, want ErrEmptyPath", err)
	}
	if _, err := StorageLeaf("a//b"); !errorIs(err, ErrInvalidPath) {
		t.Fatalf("empty-segment err = %v, want ErrInvalidPath", err)
	}
}

func TestStorageLeafDeterministic(t *testing.T) {
	a, err := StorageLeaf("财务/2024/年报.docx")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := StorageLeaf("财务/2024/年报.docx")
	if a != b {
		t.Fatalf("same path must yield same leaf: %q vs %q", a, b)
	}
}

func TestStorageLeafNoCollision(t *testing.T) {
	// Distinct logical paths must never collapse to the same leaf.
	pairs := []struct{ a, b string }{
		{"report.docx", "report.xlsx"},    // same base, different extension
		{"a/report.docx", "b/report.docx"}, // same leaf name, different directory
		{"report", "report.docx"},          // with vs without extension
		{"财务/2024/年报.docx", "预算/2024/年报.docx"}, // same leaf, different dirs (unicode)
	}
	for _, p := range pairs {
		la, err := StorageLeaf(p.a)
		if err != nil {
			t.Fatalf("StorageLeaf(%q): %v", p.a, err)
		}
		lb, err := StorageLeaf(p.b)
		if err != nil {
			t.Fatalf("StorageLeaf(%q): %v", p.b, err)
		}
		if la == lb {
			t.Fatalf("distinct paths %q and %q collided on %q", p.a, p.b, la)
		}
	}
}

func TestStorageLeafNoNormalization(t *testing.T) {
	// Case differences are NOT folded; each variant maps to its own leaf. This
	// proves the system hashes the exact bytes supplied without normalization.
	upper, _ := StorageLeaf("Report.docx")
	lower, _ := StorageLeaf("report.docx")
	if upper == lower {
		t.Fatal("case-folded variants must map to distinct leaves (no normalization)")
	}
}

func TestValidateUUID(t *testing.T) {
	cases := []struct {
		input   string
		wantErr error
	}{
		{"abc123", nil},
		{"550e8400-e29b-41d4-a716-446655440000", nil},
		{"", ErrEmptyUUID},
		{"a/b", ErrInvalidUUID},
		{"a\\b", ErrInvalidUUID},
		{"..", ErrInvalidUUID},
		{strings.Repeat("x", 256), ErrSegmentTooLong},
	}
	for _, tc := range cases {
		err := ValidateUUID(tc.input)
		if tc.wantErr == nil {
			if err != nil {
				t.Errorf("ValidateUUID(%q) unexpected err: %v", tc.input, err)
			}
			continue
		}
		if !errorIs(err, tc.wantErr) {
			t.Errorf("ValidateUUID(%q) err = %v, want %v", tc.input, err, tc.wantErr)
		}
	}
}

func TestObjectKey(t *testing.T) {
	if got := ObjectKey("abc", "leaf.docx"); got != "abc/leaf.docx" {
		t.Fatalf("ObjectKey = %q", got)
	}
}

func TestIsValidationError(t *testing.T) {
	for _, err := range []error{ErrInvalidPath, ErrEmptyPath, ErrSegmentTooLong, ErrInvalidUUID, ErrEmptyUUID} {
		if !IsValidationError(err) {
			t.Errorf("%v should be a validation error", err)
		}
	}
	if IsValidationError(ErrNotFound) {
		t.Fatal("ErrNotFound should not be a validation error")
	}
}

// errorIs is a tiny local wrapper so this test file does not need to import
// errors just for comparisons.
func errorIs(err, target error) bool {
	return err != nil && err.Error() == target.Error()
}
