// Package rpcx is the shared transport layer for blowball's internal
// scheduler/runtime RPC. It owns three concerns only:
//
//  1. Domain error codes that survive the gRPC boundary. The status message is
//     "CODE: detail"; the main service maps CODE back to its HTTP error body.
//  2. Mutual-TLS configuration plus per-method service identity authorization.
//     A caller's identity is the CommonName of its verified client
//     certificate; there is no message field or header an end user can forge.
//  3. A client interceptor that applies the configured default deadline (every
//     RPC is bounded) and converts status errors into *Error values.
//
// The package carries no business logic and no credential material. It is
// importable by the standalone Scheduler and by the main service.
package rpcx

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Domain error codes. These are the stable identifiers shared by the RPC
// boundary and the HTTP adapter. Keep them aligned with api/openapi.yaml.
const (
	CodeInvalidArgument     = "INVALID_ARGUMENT"
	CodeNotFound            = "NOT_FOUND"
	CodeConflict            = "CONFLICT"
	CodeRevisionConflict    = "REVISION_CONFLICT"
	CodeFailedPrecondition  = "FAILED_PRECONDITION"
	CodeUnauthenticated     = "UNAUTHENTICATED"
	CodePermissionDenied    = "PERMISSION_DENIED"
	CodeUnavailable         = "UNAVAILABLE"
	CodeInternal            = "INTERNAL"
	CodeDeadlineExceeded    = "DEADLINE_EXCEEDED"
	CodeResourceExhausted   = "RESOURCE_EXHAUSTED"
	CodeSessionPolicy       = "SESSION_POLICY_MIGRATION_REQUIRED"
	CodeExecutionUncertain  = "TASK_EXECUTION_UNCERTAIN"
	CodeResourceConflict    = "RESOURCE_NAMESPACE_CONFLICT"
	CodeResourceReadOnly    = "RESOURCE_READ_ONLY"
	CodeResourceLimit       = "RESOURCE_LIMIT"
	CodeInvalidModel        = "INVALID_MODEL"
	CodeInvalidEffort       = "INVALID_EFFORT"
	CodeTaskOverlap         = "TASK_OVERLAP"
	CodeSessionBusy         = "SESSION_BUSY"
	CodeUserUnavailable     = "USER_UNAVAILABLE"
	CodeStartRejected       = "START_REJECTED"
	CodeExecutionInProgress = "EXECUTION_IN_PROGRESS"
)

// Error is a domain error that survives the gRPC wire. Detail is the
// human-readable message; it must never contain a credential.
type Error struct {
	Code   string
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

func (e *Error) Unwrap() error { return e.Cause }

// Errorf builds a domain error with a formatted detail.
func Errorf(code, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Wrap builds a domain error that retains cause for logs.
func Wrap(code, detail string, cause error) error {
	return &Error{Code: code, Detail: detail, Cause: cause}
}

// AsError returns the *Error in err's chain, if any.
func AsError(err error) (*Error, bool) {
	return errors.AsType[*Error](err)
}

// grpcCode maps a domain code onto the transport status family.
func grpcCode(code string) codes.Code {
	switch code {
	case CodeInvalidArgument, CodeInvalidModel, CodeInvalidEffort:
		return codes.InvalidArgument
	case CodeNotFound:
		return codes.NotFound
	case CodeConflict, CodeRevisionConflict, CodeFailedPrecondition,
		CodeSessionPolicy, CodeExecutionUncertain, CodeResourceConflict,
		CodeResourceReadOnly, CodeTaskOverlap, CodeSessionBusy,
		CodeUserUnavailable, CodeStartRejected, CodeExecutionInProgress:
		return codes.FailedPrecondition
	case CodeUnauthenticated:
		return codes.Unauthenticated
	case CodePermissionDenied:
		return codes.PermissionDenied
	case CodeUnavailable:
		return codes.Unavailable
	case CodeDeadlineExceeded:
		return codes.DeadlineExceeded
	case CodeResourceExhausted, CodeResourceLimit:
		return codes.ResourceExhausted
	default:
		return codes.Internal
	}
}

// domainCode is the inverse mapping for statuses that did not carry a domain
// prefix (e.g. produced by grpc itself).
func domainCode(c codes.Code) string {
	switch c {
	case codes.InvalidArgument:
		return CodeInvalidArgument
	case codes.NotFound:
		return CodeNotFound
	case codes.AlreadyExists, codes.Aborted:
		return CodeConflict
	case codes.FailedPrecondition:
		return CodeFailedPrecondition
	case codes.Unauthenticated:
		return CodeUnauthenticated
	case codes.PermissionDenied:
		return CodePermissionDenied
	case codes.Unavailable:
		return CodeUnavailable
	case codes.DeadlineExceeded:
		return CodeDeadlineExceeded
	case codes.ResourceExhausted:
		return CodeResourceExhausted
	default:
		return CodeInternal
	}
}

// ToStatus converts a domain error into a gRPC status error. A non-domain
// error becomes codes.Internal with the INTERNAL prefix; an existing status
// error passes through.
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if de, ok := errors.AsType[*Error](err); ok {
		msg := de.Code
		if de.Detail != "" {
			msg += ": " + de.Detail
		}
		// The cause stays local (logs); the wire carries only the stable code
		// and the sanitized detail.
		return status.Error(grpcCode(de.Code), msg)
	}
	return status.Error(codes.Internal, CodeInternal+": "+err.Error())
}

// FromStatus converts a gRPC status error into a domain *Error so callers can
// branch on stable codes. It returns nil for a nil error and a wrapped
// *Error (with the original status as Cause) otherwise.
func FromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	msg := st.Message()
	if i := strings.Index(msg, ": "); i > 0 {
		candidate := msg[:i]
		if isDomainCode(candidate) {
			return &Error{Code: candidate, Detail: msg[i+2:], Cause: err}
		}
	}
	return &Error{Code: domainCode(st.Code()), Detail: msg, Cause: err}
}

func isDomainCode(v string) bool {
	switch v {
	case CodeInvalidArgument, CodeNotFound, CodeConflict, CodeRevisionConflict,
		CodeFailedPrecondition, CodeUnauthenticated, CodePermissionDenied,
		CodeUnavailable, CodeInternal, CodeDeadlineExceeded,
		CodeResourceExhausted, CodeSessionPolicy, CodeExecutionUncertain,
		CodeResourceConflict, CodeResourceReadOnly, CodeResourceLimit,
		CodeInvalidModel, CodeInvalidEffort, CodeTaskOverlap, CodeSessionBusy,
		CodeUserUnavailable, CodeStartRejected, CodeExecutionInProgress:
		return true
	default:
		return false
	}
}

// HTTPStatus maps an error to the main service's HTTP error contract:
// 400/401/403/404/409/429/500/503. Unknown errors are treated as 500.
func HTTPStatus(err error) (statusCode int, code string, message string) {
	if err == nil {
		return 200, "", ""
	}
	de, ok := AsError(err)
	if !ok {
		return 500, CodeInternal, err.Error()
	}
	switch de.Code {
	case CodeInvalidArgument, CodeInvalidModel, CodeInvalidEffort:
		return 400, de.Code, de.Detail
	case CodeUnauthenticated:
		return 401, de.Code, de.Detail
	case CodePermissionDenied:
		return 403, de.Code, de.Detail
	case CodeNotFound:
		return 404, de.Code, de.Detail
	case CodeConflict, CodeRevisionConflict, CodeFailedPrecondition,
		CodeSessionPolicy, CodeExecutionUncertain, CodeResourceConflict,
		CodeResourceReadOnly, CodeTaskOverlap, CodeSessionBusy,
		CodeUserUnavailable, CodeStartRejected, CodeExecutionInProgress:
		return 409, de.Code, de.Detail
	case CodeResourceExhausted, CodeResourceLimit:
		return 429, de.Code, de.Detail
	case CodeUnavailable:
		return 503, de.Code, de.Detail
	case CodeDeadlineExceeded:
		return 504, de.Code, de.Detail
	default:
		return 500, de.Code, de.Detail
	}
}

// IsUnavailable reports whether the error is the fail-closed dependency error
// the HTTP layer must surface as 503 (never a local fallback).
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	de, ok := AsError(err)
	return ok && de.Code == CodeUnavailable
}

// ContextCheck is a tiny helper for tests and call sites that need to assert a
// deadline is present without importing grpc metadata helpers.
func ContextCheck(ctx context.Context) bool {
	_, ok := ctx.Deadline()
	return ok
}
