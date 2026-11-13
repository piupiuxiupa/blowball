package mcpclient

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

// maxMessageBytes bounds a single MCP message: one SSE `data:` line for the
// Streamable HTTP transport, one newline-delimited JSON-RPC line for the SSE
// and stdio transports. It replaces the implicit 1 MiB bufio.Scanner token cap
// that previously turned large tool results (big file reads, base64 blobs, huge
// query output) into an opaque `bufio.Scanner: token too long` error. 32 MiB
// leaves ample headroom for large legitimate results while keeping a hard
// ceiling against runaway or malicious servers.
const maxMessageBytes int64 = 32 << 20 // 32 MiB

// errMessageTooLarge is returned when a single message/line exceeds the cap. It
// is wrapped with the limit so callers get a clear, locatable error instead of
// bufio's internal token-too-long.
var errMessageTooLarge = errors.New("mcp message too large")

// readCappedLine reads one line (up to and excluding the first '\n'; a trailing
// '\r' is also stripped) from br. It bounds memory incrementally — if the line
// exceeds maxBytes before a newline is found it returns errMessageTooLarge
// (wrapped with the limit) and discards any partial data. At a clean end of
// stream it returns ("", io.EOF); at EOF with a final partial line (no trailing
// newline) it returns that partial line together with io.EOF so the caller can
// still process it. Other read errors are returned alongside any partial line.
//
// Unlike bufio.Scanner it imposes no fixed token limit, so arbitrarily long
// lines are read successfully up to maxBytes.
func readCappedLine(br *bufio.Reader, maxBytes int64) (string, error) {
	var sb strings.Builder
	for {
		b, err := br.ReadByte()
		if err != nil {
			if sb.Len() == 0 {
				return "", err
			}
			return sb.String(), err
		}
		if b == '\n' {
			return strings.TrimSuffix(sb.String(), "\r"), nil
		}
		sb.WriteByte(b)
		if int64(sb.Len()) > maxBytes {
			return "", fmt.Errorf("%w (exceeds %d bytes)", errMessageTooLarge, maxBytes)
		}
	}
}
