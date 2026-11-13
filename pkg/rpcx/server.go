package rpcx

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Full gRPC method names. Keeping them as constants makes the authorizer map
// greppable and prevents typo-driven fail-open policies.
const (
	MethodCreateTask = "/blowball.scheduler.v1.SchedulerService/CreateTask"
	MethodGetTask    = "/blowball.scheduler.v1.SchedulerService/GetTask"
	MethodListTasks  = "/blowball.scheduler.v1.SchedulerService/ListTasks"
	MethodUpdateTask = "/blowball.scheduler.v1.SchedulerService/UpdateTask"
	MethodDeleteTask = "/blowball.scheduler.v1.SchedulerService/DeleteTask"

	MethodListOccurrences       = "/blowball.scheduler.v1.SchedulerService/ListOccurrences"
	MethodGetOccurrence         = "/blowball.scheduler.v1.SchedulerService/GetOccurrence"
	MethodAuthorizeStart        = "/blowball.scheduler.v1.SchedulerService/AuthorizeStart"
	MethodGetStartGrant         = "/blowball.scheduler.v1.SchedulerService/GetStartGrant"
	MethodGetResourceReferences = "/blowball.scheduler.v1.SchedulerService/GetResourceReferences"

	MethodValidateTaskActorAndConfig = "/blowball.scheduler.v1.SchedulerControlService/ValidateTaskActorAndConfig"
	MethodValidateResourceBinding    = "/blowball.scheduler.v1.SchedulerControlService/ValidateResourceBinding"
	MethodGetResourceVersion         = "/blowball.scheduler.v1.SchedulerControlService/GetResourceVersion"
	MethodGetResourceDraft           = "/blowball.scheduler.v1.SchedulerControlService/GetResourceDraft"
	MethodPublishResource            = "/blowball.scheduler.v1.SchedulerControlService/PublishResource"

	MethodSubmitScheduledRun    = "/blowball.scheduledruntime.v1.ScheduledRuntimeService/SubmitScheduledRun"
	MethodGetScheduledExecution = "/blowball.scheduledruntime.v1.ScheduledRuntimeService/GetScheduledExecution"
	MethodCancelScheduledRun    = "/blowball.scheduledruntime.v1.ScheduledRuntimeService/CancelScheduledRun"
)

const healthServicePrefix = "/grpc.health.v1.Health/"

// MethodAuthorizer maps a full method name to the service identities allowed
// to call it. A method absent from the map is denied (fail closed).
type MethodAuthorizer map[string][]string

// DefaultAuthorizer is the deployment-wide policy:
//   - Scheduler management/history/grant RPCs are called by the main service
//     roles (API, Agent for tools, combined main).
//   - Main-service control RPCs are called by the Scheduler (validation and
//     resource reads) and by main-service roles (agent tools).
//   - Runtime RPCs are called only by the Scheduler (plus the combined main
//     role for in-process tests/operations).
func DefaultAuthorizer() MethodAuthorizer {
	main := []string{IdentityAPI, IdentityAgent, IdentityMain}
	schedulerAndMain := []string{IdentityScheduler, IdentityAPI, IdentityAgent, IdentityMain}
	return MethodAuthorizer{
		MethodCreateTask: main,
		MethodGetTask:    main,
		MethodListTasks:  main,
		MethodUpdateTask: main,
		MethodDeleteTask: main,

		MethodListOccurrences: main,
		// The occurrence read and the start fence are called by the Runtime
		// (agent/all) and by the API role's scheduled-session detail lookup.
		MethodGetOccurrence:         []string{IdentityScheduler, IdentityAPI, IdentityAgent, IdentityMain},
		MethodAuthorizeStart:        []string{IdentityScheduler, IdentityAgent, IdentityMain},
		MethodGetStartGrant:         []string{IdentityScheduler, IdentityAgent, IdentityMain},
		MethodGetResourceReferences: schedulerAndMain,

		MethodValidateTaskActorAndConfig: schedulerAndMain,
		MethodValidateResourceBinding:    schedulerAndMain,
		MethodGetResourceVersion:         schedulerAndMain,
		MethodGetResourceDraft:           schedulerAndMain,
		MethodPublishResource:            []string{IdentityAPI, IdentityAgent, IdentityMain},

		MethodSubmitScheduledRun:    []string{IdentityScheduler, IdentityMain},
		MethodGetScheduledExecution: []string{IdentityScheduler, IdentityAPI, IdentityAgent, IdentityMain},
		MethodCancelScheduledRun:    []string{IdentityScheduler, IdentityAPI, IdentityAgent, IdentityMain},
	}
}

// Allowed reports whether identity may call fullMethod.
func (a MethodAuthorizer) Allowed(fullMethod, identity string) bool {
	return slices.Contains(a[fullMethod], identity)
}

// ServerOptions configures the internal RPC server and its interceptors.
type ServerOptions struct {
	SecurityMode SecurityMode
	TLS          TLSFiles
	PeerTokens   map[string]string
	// Authorizer defaults to DefaultAuthorizer when nil.
	Authorizer MethodAuthorizer
	// RequireDeadline, when true (the production setting), rejects calls that
	// arrive without a deadline. Health checks are exempt.
	RequireDeadline bool
}

// NewServer constructs an authenticated internal RPC listener.
func NewServer(opts ServerOptions) (*grpc.Server, error) {
	mode, err := NormalizeSecurityMode(opts.SecurityMode)
	if err != nil {
		return nil, err
	}
	opts.SecurityMode = mode
	serverOpts := []grpc.ServerOption{}
	if mode == SecurityModeMTLS {
		tlsCfg, err := ServerTLS(opts.TLS)
		if err != nil {
			return nil, err
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	} else if err := ValidatePeerTokens(opts.PeerTokens); err != nil {
		return nil, err
	}
	serverOpts = append(serverOpts,
		grpc.UnaryInterceptor(UnaryServerInterceptor(opts)),
		grpc.StreamInterceptor(StreamServerInterceptor(opts)),
	)
	return grpc.NewServer(serverOpts...), nil
}

// UnaryServerInterceptor enforces authenticated service identity, the per-method
// authorization policy, a deadline, and panic containment. It runs before any
// handler, so an unauthorized request can never read user data or start an
// agent.
func UnaryServerInterceptor(opts ServerOptions) grpc.UnaryServerInterceptor {
	authorize := serverAuthorization(opts)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				resp, err = nil, status.Error(codes.Internal, CodeInternal+": internal rpc panic")
			}
		}()

		if err := authorize(ctx, info.FullMethod); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamServerInterceptor applies the same authorization and panic containment
// to streaming RPCs, including the health service's Watch method.
func StreamServerInterceptor(opts ServerOptions) grpc.StreamServerInterceptor {
	authorize := serverAuthorization(opts)
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = status.Error(codes.Internal, CodeInternal+": internal rpc panic")
			}
		}()

		if err := authorize(stream.Context(), info.FullMethod); err != nil {
			return err
		}
		return handler(srv, stream)
	}
}

func serverAuthorization(opts ServerOptions) func(context.Context, string) error {
	mode, modeErr := NormalizeSecurityMode(opts.SecurityMode)
	if modeErr == nil && mode == SecurityModePlaintextToken {
		modeErr = ValidatePeerTokens(opts.PeerTokens)
		opts.PeerTokens = maps.Clone(opts.PeerTokens)
	}
	authorizer := opts.Authorizer
	if authorizer == nil {
		authorizer = DefaultAuthorizer()
	}
	return func(ctx context.Context, fullMethod string) error {
		if modeErr != nil {
			return status.Error(codes.Unauthenticated, CodeUnauthenticated+": invalid rpc security configuration")
		}
		var identity string
		if mode == SecurityModePlaintextToken {
			var ok bool
			identity, ok = tokenIdentity(ctx, opts.PeerTokens)
			if !ok {
				return status.Error(codes.Unauthenticated, CodeUnauthenticated+": service token required")
			}
		}

		// Health checks still require mTLS at the transport or a verified token,
		// are exempt from the deadline and per-method policy so an orchestrator
		// can probe liveness cheaply.
		if strings.HasPrefix(fullMethod, healthServicePrefix) {
			return nil
		}

		if opts.RequireDeadline {
			if _, ok := ctx.Deadline(); !ok {
				return status.Error(codes.InvalidArgument, CodeInvalidArgument+": deadline required")
			}
		}

		if mode == SecurityModeMTLS {
			var ok bool
			identity, ok = PeerIdentity(ctx)
			if !ok || identity == "" {
				return status.Error(codes.Unauthenticated, CodeUnauthenticated+": client certificate identity required")
			}
		}
		if !authorizer.Allowed(fullMethod, identity) {
			return status.Error(codes.PermissionDenied, CodePermissionDenied+": service identity not authorized for method")
		}
		return nil
	}
}

// PeerIdentity returns the verified client certificate's CommonName from the
// transport credentials. It never consults request metadata.
func PeerIdentity(ctx context.Context) (string, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return "", false
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", false
	}
	if len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return "", false
	}
	return CertificateIdentity(info.State.VerifiedChains[0][0]), true
}

// UnaryClientInterceptor applies the configured default deadline when the
// caller has none and converts transport statuses back into domain errors.
func UnaryClientInterceptor(defaultTimeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if defaultTimeout > 0 {
			if _, ok := ctx.Deadline(); !ok {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
				defer cancel()
			}
		}
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err == nil {
			return nil
		}
		return FromStatus(err)
	}
}
