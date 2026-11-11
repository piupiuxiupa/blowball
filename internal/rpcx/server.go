package rpcx

import (
	"context"
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

	MethodListOccurrences = "/blowball.scheduler.v1.SchedulerService/ListOccurrences"
	MethodGetOccurrence   = "/blowball.scheduler.v1.SchedulerService/GetOccurrence"
	MethodAuthorizeStart  = "/blowball.scheduler.v1.SchedulerService/AuthorizeStart"
	MethodGetStartGrant   = "/blowball.scheduler.v1.SchedulerService/GetStartGrant"

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
		MethodGetOccurrence:   []string{IdentityScheduler, IdentityMain},
		MethodAuthorizeStart:  []string{IdentityScheduler, IdentityMain},
		MethodGetStartGrant:   []string{IdentityScheduler, IdentityMain},

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

// ServerOptions configures the unary server interceptor.
type ServerOptions struct {
	// Authorizer defaults to DefaultAuthorizer when nil.
	Authorizer MethodAuthorizer
	// RequireDeadline, when true (the production setting), rejects calls that
	// arrive without a deadline. Health checks are exempt.
	RequireDeadline bool
}

// UnaryServerInterceptor enforces mTLS service identity, the per-method
// authorization policy, a deadline, and panic containment. It runs before any
// handler, so an unauthorized request can never read user data or start an
// agent.
func UnaryServerInterceptor(opts ServerOptions) grpc.UnaryServerInterceptor {
	authorizer := opts.Authorizer
	if authorizer == nil {
		authorizer = DefaultAuthorizer()
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				resp, err = nil, status.Error(codes.Internal, CodeInternal+": internal rpc panic")
			}
		}()

		// Health checks are infrastructure probes: they still require mTLS but
		// are exempt from the deadline and per-method policy so an orchestrator
		// can probe liveness cheaply.
		if strings.HasPrefix(info.FullMethod, healthServicePrefix) {
			return handler(ctx, req)
		}

		if opts.RequireDeadline {
			if _, ok := ctx.Deadline(); !ok {
				return nil, status.Error(codes.InvalidArgument, CodeInvalidArgument+": deadline required")
			}
		}

		identity, ok := PeerIdentity(ctx)
		if !ok || identity == "" {
			return nil, status.Error(codes.Unauthenticated, CodeUnauthenticated+": client certificate identity required")
		}
		if !authorizer.Allowed(info.FullMethod, identity) {
			return nil, status.Error(codes.PermissionDenied, CodePermissionDenied+": service identity not authorized for method")
		}
		return handler(ctx, req)
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
