package mcpclient

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadCappedLine_ReadsFullLine(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("hello world\nnext\n"))
	line, err := readCappedLine(br, 1024)
	require.NoError(t, err)
	require.Equal(t, "hello world", line)
}

func TestReadCappedLine_StripsCRLF(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("abc\r\n"))
	line, err := readCappedLine(br, 1024)
	require.NoError(t, err)
	require.Equal(t, "abc", line)
}

func TestReadCappedLine_ExceedsLimit(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(strings.Repeat("a", 100) + "\n"))
	_, err := readCappedLine(br, 16)
	require.Error(t, err)
	require.ErrorIs(t, err, errMessageTooLarge)
	require.Contains(t, err.Error(), "16")
	// The opaque scanner error must not leak through.
	require.NotContains(t, err.Error(), "token too long")
}

func TestReadCappedLine_CleanEOF(t *testing.T) {
	br := bufio.NewReader(strings.NewReader(""))
	line, err := readCappedLine(br, 1024)
	require.Equal(t, "", line)
	require.ErrorIs(t, err, io.EOF)
}

func TestReadCappedLine_PartialLineAtEOF(t *testing.T) {
	br := bufio.NewReader(strings.NewReader("partial-no-newline"))
	line, err := readCappedLine(br, 1024)
	require.Equal(t, "partial-no-newline", line)
	require.ErrorIs(t, err, io.EOF)
}
