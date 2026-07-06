package executor

import (
	"bytes"
	"testing"
)

func TestDetectDangerousCommand(t *testing.T) {
	cases := []struct {
		cmd      string
		dangerous bool
	}{
		{"rm -rf /workspace/build", true},
		{"curl http://example.com", true},
		{"wget https://example.com/file", true},
		{"sudo apt update", true},
		{"sshd -D", true},
		{"echo hello", false},
		{"go test ./...", false},
		{"python train.py", false},
		{"git rm old.go", true},
	}

	for _, tc := range cases {
		got := detectDangerousCommand(tc.cmd)
		if got != tc.dangerous {
			t.Errorf("detectDangerousCommand(%q) = %v, want %v", tc.cmd, got, tc.dangerous)
		}
	}
}

func TestTruncateOutputNoTruncation(t *testing.T) {
	input := []byte("hello world")
	out, truncated := truncateOutput(input, 100)
	if truncated {
		t.Error("expected no truncation")
	}
	if out != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", out)
	}
}

func TestTruncateOutputTruncates(t *testing.T) {
	input := []byte("hello world")
	out, truncated := truncateOutput(input, 5)
	if !truncated {
		t.Error("expected truncation")
	}
	want := "hello" + truncationMarker
	if out != want {
		t.Errorf("expected %q, got %q", want, out)
	}
}

func TestTruncateOutputZeroMax(t *testing.T) {
	input := []byte("hello")
	out, truncated := truncateOutput(input, 0)
	if truncated {
		t.Error("expected no truncation with max 0")
	}
	if out != "hello" {
		t.Errorf("expected %q, got %q", "hello", out)
	}
}

func TestMaxBytesWriter(t *testing.T) {
	w := &maxBytesWriter{max: 5}
	n, err := w.Write([]byte("abc"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 bytes written, got %d", n)
	}

	n, err = w.Write([]byte("def"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 bytes reported, got %d", n)
	}

	if !bytes.Equal(w.buf.Bytes(), []byte("abcde")) {
		t.Errorf("expected abcde, got %q", w.buf.Bytes())
	}
}

func TestMaxBytesWriterUnlimited(t *testing.T) {
	w := &maxBytesWriter{max: 0}
	data := []byte("hello world")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != len(data) {
		t.Errorf("expected %d bytes, got %d", len(data), n)
	}
	if !bytes.Equal(w.buf.Bytes(), data) {
		t.Errorf("unexpected buffer content: %q", w.buf.Bytes())
	}
}
