package rpcx

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestErrorRoundTripPreservesDomainCode(t *testing.T) {
	cases := []struct {
		code string
		grpc codes.Code
	}{
		{CodeInvalidArgument, codes.InvalidArgument},
		{CodeNotFound, codes.NotFound},
		{CodeRevisionConflict, codes.FailedPrecondition},
		{CodeSessionPolicy, codes.FailedPrecondition},
		{CodeExecutionUncertain, codes.FailedPrecondition},
		{CodeResourceReadOnly, codes.FailedPrecondition},
		{CodePermissionDenied, codes.PermissionDenied},
		{CodeUnavailable, codes.Unavailable},
		{CodeResourceLimit, codes.ResourceExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			wire := ToStatus(Errorf(tc.code, "detail %d", 7))
			st, ok := status.FromError(wire)
			require.True(t, ok)
			assert.Equal(t, tc.grpc, st.Code())

			back := FromStatus(wire)
			de, ok := AsError(back)
			require.True(t, ok)
			assert.Equal(t, tc.code, de.Code)
			assert.Equal(t, "detail 7", de.Detail)
		})
	}
}

func TestToStatusSanitizesUnknownErrors(t *testing.T) {
	err := ToStatus(errors.New("boom"))
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
	assert.Contains(t, st.Message(), CodeInternal)
}

func TestFromStatusWithoutDomainPrefix(t *testing.T) {
	de, ok := AsError(FromStatus(status.Error(codes.Unavailable, "connection refused")))
	require.True(t, ok)
	assert.Equal(t, CodeUnavailable, de.Code)
	assert.Equal(t, "connection refused", de.Detail)

	de, ok = AsError(FromStatus(status.Error(codes.Aborted, "aborted")))
	require.True(t, ok)
	assert.Equal(t, CodeConflict, de.Code)
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{CodeInvalidArgument, 400},
		{CodeInvalidModel, 400},
		{CodeUnauthenticated, 401},
		{CodePermissionDenied, 403},
		{CodeNotFound, 404},
		{CodeRevisionConflict, 409},
		{CodeExecutionUncertain, 409},
		{CodeResourceLimit, 429},
		{CodeUnavailable, 503},
	}
	for _, tc := range cases {
		got, code, _ := HTTPStatus(Errorf(tc.code, "x"))
		assert.Equal(t, tc.want, got, tc.code)
		assert.Equal(t, tc.code, code)
	}
}

func TestHTTPStatusUnknownIs500(t *testing.T) {
	got, code, _ := HTTPStatus(errors.New("plain"))
	assert.Equal(t, 500, got)
	assert.Equal(t, CodeInternal, code)
}

func TestIsUnavailable(t *testing.T) {
	assert.True(t, IsUnavailable(Errorf(CodeUnavailable, "down")))
	assert.False(t, IsUnavailable(Errorf(CodeNotFound, "missing")))
	assert.False(t, IsUnavailable(nil))
}
