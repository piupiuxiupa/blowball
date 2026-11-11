package rpcx

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// DefaultTimeout bounds every internal RPC unless the caller set a shorter (or
// explicit longer) deadline.
const DefaultTimeout = 5 * time.Second

// DialOptions configures the shared client connection.
type DialOptions struct {
	// Target is a gRPC target; a bare host:port is prefixed with dns:///.
	Target string
	// TLS carries the client certificate and CA. Required.
	TLS TLSFiles
	// Timeout is the default RPC deadline (DefaultTimeout when zero).
	Timeout time.Duration
	// Extra options are appended after the ours (tests use bufconn dialers).
	Extra []grpc.DialOption
}

// Dial creates one reusable lazy client connection. Connections are safe for
// concurrent use; wiring constructs one per endpoint and shares it across
// every RPC client in the process. A Scheduler outage therefore surfaces as a
// per-call UNAVAILABLE error (HTTP 503), never as a startup failure.
func Dial(opts DialOptions) (*grpc.ClientConn, error) {
	tlsCfg, err := ClientTLS(opts.TLS)
	if err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	target := opts.Target
	if target == "" {
		return nil, fmt.Errorf("rpcx: target is required")
	}
	if !strings.Contains(target, "://") {
		target = "dns:///" + target
	}
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithChainUnaryInterceptor(UnaryClientInterceptor(timeout)),
	}
	dialOpts = append(dialOpts, opts.Extra...)
	return grpc.NewClient(target, dialOpts...)
}

// DialBare is a test/support helper for an already-authenticated in-memory
// listener (bufconn). Production callers use Dial.
func DialBare(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return grpc.NewClient(target, opts...)
}
