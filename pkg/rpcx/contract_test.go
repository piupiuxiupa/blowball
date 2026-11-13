package rpcx_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	scheduledruntimev1 "github.com/lush/blowball/api/gen/go/scheduledruntime/v1"
	schedulerv1 "github.com/lush/blowball/api/gen/go/scheduler/v1"
)

// TestProtoFieldRoundTrip covers every field of the two wire-critical messages
// so a renamed/dropped field cannot slip through codegen unnoticed.
func TestProtoFieldRoundTrip(t *testing.T) {
	now := timestamppb.Now()
	occ := &schedulerv1.Occurrence{
		OccurrenceId:     "occ-1",
		TaskId:           "task-1",
		UserId:           "user-1",
		TaskRevision:     7,
		SnapshotJson:     `{"prompt":"x"}`,
		ScheduledFor:     now,
		RunId:            "run-1",
		SessionId:        "sess-1",
		SessionSource:    schedulerv1.SessionSource_SESSION_SOURCE_SCHEDULED,
		Status:           "pending",
		Reason:           "",
		AttemptCount:     2,
		StartedAt:        now,
		FinishedAt:       now,
		LastError:        "boom",
		ResourceRevision: 3,
		ManifestHash:     strings.Repeat("a", 64),
	}
	raw, err := proto.Marshal(occ)
	require.NoError(t, err)
	var back schedulerv1.Occurrence
	require.NoError(t, proto.Unmarshal(raw, &back))
	assert.True(t, proto.Equal(occ, &back))

	grant := &schedulerv1.StartGrant{
		OccurrenceId: "occ-1", Generation: 4, RunId: "run-1", SessionId: "sess-1",
		StartedAt: now, Deadline: now,
	}
	raw, err = proto.Marshal(grant)
	require.NoError(t, err)
	var grantBack schedulerv1.StartGrant
	require.NoError(t, proto.Unmarshal(raw, &grantBack))
	assert.True(t, proto.Equal(grant, &grantBack))

	exec := &scheduledruntimev1.ScheduledExecution{
		OccurrenceId: "occ-1", TaskId: "task-1", UserId: "user-1", RunId: "run-1",
		SessionId: "sess-1", ReceiptGeneration: 2, Status: "uncertain", Reason: "RESULT_UNKNOWN",
		LastError: "x", AcceptedAt: now, StartIntentAt: now, StartedAt: now, FinishedAt: now,
		ConfigFingerprint: "fp", GrantGeneration: 2, GrantDeadline: now,
		Terminal: false, Started: true, Uncertain: true,
		StopProof: &scheduledruntimev1.ExecutionStopProof{
			Present: true, OccurrenceId: "occ-1", RunId: "run-1", Generation: 2, ProvenAt: now, Kind: "process_exit",
		},
	}
	raw, err = proto.Marshal(exec)
	require.NoError(t, err)
	var execBack scheduledruntimev1.ScheduledExecution
	require.NoError(t, proto.Unmarshal(raw, &execBack))
	assert.True(t, proto.Equal(exec, &execBack))
}

// TestProtoUnknownFieldsAreTolerated pins forward compatibility: a newer peer
// may add fields; an older peer must ignore them.
func TestProtoUnknownFieldsAreTolerated(t *testing.T) {
	base := &schedulerv1.Task{TaskId: "task-1", UserId: "user-1", Revision: 2}
	raw, err := proto.Marshal(base)
	require.NoError(t, err)
	// Field 999, varint value 42 (an unknown field in any current version).
	raw = protowire.AppendTag(raw, 999, protowire.VarintType)
	raw = protowire.AppendVarint(raw, 42)

	var back schedulerv1.Task
	require.NoError(t, proto.Unmarshal(raw, &back))
	assert.Equal(t, "task-1", back.GetTaskId())
	assert.Equal(t, int64(2), back.GetRevision())
}

// TestProtoPackagesAreVersioned protects the /v1 contract path.
func TestProtoPackagesAreVersioned(t *testing.T) {
	assert.Equal(t, "blowball.scheduler.v1", string(schedulerv1.File_scheduler_v1_scheduler_proto.Package()))
	assert.Equal(t, "blowball.scheduledruntime.v1", string(scheduledruntimev1.File_scheduledruntime_v1_scheduled_runtime_proto.Package()))
}

// TestProtoCarriesNoCredentialFields enforces the "secrets never cross the
// RPC boundary" invariant structurally: no message field may name a
// credential. This catches a well-meaning future addition before it ships.
func TestProtoCarriesNoCredentialFields(t *testing.T) {
	forbidden := []string{"token", "secret", "password", "credential", "api_key", "apikey", "authorization", "jwt", "private_key", "bearer"}
	files := []protoreflect.FileDescriptor{
		schedulerv1.File_scheduler_v1_scheduler_proto,
		scheduledruntimev1.File_scheduledruntime_v1_scheduled_runtime_proto,
	}
	var walk func(prefix string, msgs protoreflect.MessageDescriptors)
	walk = func(prefix string, msgs protoreflect.MessageDescriptors) {
		for i := 0; i < msgs.Len(); i++ {
			msg := msgs.Get(i)
			name := prefix + string(msg.Name())
			fields := msg.Fields()
			for j := 0; j < fields.Len(); j++ {
				field := strings.ToLower(string(fields.Get(j).Name()))
				for _, bad := range forbidden {
					assert.NotContains(t, field, bad,
						"proto message %s exposes credential-like field %q", name, field)
				}
			}
			walk(name+".", msg.Messages())
		}
	}
	for _, file := range files {
		walk("", file.Messages())
	}
}
