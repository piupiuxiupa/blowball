package rpcx

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// DefaultTimeout bounds every internal RPC unless the caller set a shorter (or
// explicit longer) deadline.
const DefaultTimeout = 5 * time.Second

// DialOptions configures the shared client connection.
type DialOptions struct {
	// Target is a gRPC target; a bare host:port is prefixed with dns:///.
	Target string
	// TLS carries the client certificate and CA. Required for mTLS.
	TLS TLSFiles
	// SecurityMode defaults to mTLS. Token authentication is explicit.
	SecurityMode SecurityMode
	ClientToken  string
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
	mode, err := NormalizeSecurityMode(opts.SecurityMode)
	if err != nil {
		return nil, err
	}
	dialOpts := []grpc.DialOption{}
	if mode == SecurityModeMTLS {
		tlsCfg, err := ClientTLS(opts.TLS)
		if err != nil {
			return nil, err
		}
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		if err := ValidateClientToken(opts.ClientToken); err != nil {
			return nil, err
		}
		dialOpts = append(dialOpts,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(tokenCredentials{token: opts.ClientToken}),
		)
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
	dialOpts = append(dialOpts, grpc.WithChainUnaryInterceptor(UnaryClientInterceptor(timeout)))
	dialOpts = append(dialOpts, opts.Extra...)
	return grpc.NewClient(target, dialOpts...)
}
