package rpc

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lush/blowball/pkg/rpcx"
	"github.com/lush/blowball/services/scheduler/internal/store"
	"github.com/lush/blowball/services/scheduler/internal/task"
)

func TestActorIsRequired(t *testing.T) {
	_, err := actorOf(nil)
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	_, err = actorOf(nil)
	require.Error(t, err)
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code codes.Code
	}{
		{"validation", &task.ValidationError{Code: "INVALID_NAME", Message: "bad"}, codes.InvalidArgument},
		{"not found", task.ErrNotFound, codes.NotFound},
		{"revision conflict", task.ErrRevisionConflict, codes.FailedPrecondition},
		{"task unavailable", store.ErrTaskUnavailable, codes.FailedPrecondition},
		{"grant fenced", store.ErrGrantFenced, codes.FailedPrecondition},
		{"occurrence missing", store.ErrOccurrenceNotFound, codes.NotFound},
		{"grant missing", store.ErrGrantNotFound, codes.NotFound},
		{"domain passthrough", rpcx.Errorf(rpcx.CodeInvalidModel, "unknown model"), codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapped := rpcErr(tc.err)
			require.Error(t, mapped)
			assert.Equal(t, tc.code, status.Code(mapped))
		})
	}
}

func TestErrorMappingDefaultIsUnavailable(t *testing.T) {
	assert.Equal(t, codes.Unavailable, status.Code(rpcErr(errors.New("db down"))))
}
