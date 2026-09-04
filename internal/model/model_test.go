package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// expectedDBTags maps a struct type name to the ordered list of `db:` tag
// values its fields must expose. Keeping this in one place makes the test a
// faithful mirror of the migration schemas.
var expectedDBTags = map[string][]string{
	"User": {
		"user_id", "username", "password", "status", "update_time", "create_time",
	},
	"Session": {
		"session_id", "user_id", "trace_id",
		"update_time", "create_time", "context_compacted",
	},
	"Title": {
		"session_id", "title", "trace_id", "is_manual",
		"update_time", "create_time",
	},
	"Message": {
		"id", "session_id", "msg_time", "agent", "msg_index",
		"role", "event_type", "content", "trace_id", "client_msg_id", "run_id",
		"agent_instance_id", "update_time",
	},
	"TurnUsage": {
		"id", "session_id", "trace_id", "user_id", "usage_json",
		"total_tokens", "model", "context_tokens", "created_at",
	},
	"ContextCompaction": {
		"id", "session_id", "user_id", "trace_id", "trigger_kind",
		"trigger_tokens", "content", "boundary_msg_time", "boundary_msg_index",
		"boundary_msg_id", "shadowed_tokens", "summary_model",
		"summary_prompt_tokens", "summary_completion_tokens", "create_time",
	},
}

// structTypes returns the model structs covered by the test.
func structTypes() map[string]any {
	return map[string]any{
		"User":              User{},
		"Session":           Session{},
		"Title":             Title{},
		"Message":           Message{},
		"TurnUsage":         TurnUsage{},
		"ContextCompaction": ContextCompaction{},
	}
}

func TestStructs_HaveExpectedDBTags(t *testing.T) {
	for name, val := range structTypes() {
		t.Run(name, func(t *testing.T) {
			v := reflect.ValueOf(val)
			typ := v.Type()
			want := expectedDBTags[name]

			if got := typ.NumField(); got != len(want) {
				t.Fatalf("%s has %d fields, expected %d", name, got, len(want))
			}

			for i, wantTag := range want {
				f := typ.Field(i)
				got := f.Tag.Get("db")
				if got != wantTag {
					t.Errorf("%s field %d (%s): db tag = %q, want %q",
						name, i, f.Name, got, wantTag)
				}
			}
		})
	}
}

func TestStructs_JSONTagsRoundTrip(t *testing.T) {
	// Every field except User.Password (json:"-") must marshal under its
	// snake_case db name. We assert the JSON payload contains the expected
	// key set and, importantly, that User.Password is never emitted.
	cases := []struct {
		name   string
		target any
		want   []string
		skip   []string // keys that must NOT appear
	}{
		{
			name: "User",
			target: User{
				UserID: "u-1", Username: "alice", Password: "secret",
				Status:     "active",
				UpdateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
				CreateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
			},
			want: []string{"user_id", "username", "status", "update_time", "create_time"},
			skip: []string{"password"},
		},
		{
			name: "Session",
			target: Session{
				SessionID: "s-1", UserID: "u-1", TraceID: "t-1",
				UpdateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
				CreateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
			},
			want: []string{"session_id", "user_id", "trace_id", "update_time", "create_time", "context_compacted"},
		},
		{
			name: "Title",
			target: Title{
				SessionID: "s-1", Title: "hello", TraceID: "t-1", IsManual: true,
				UpdateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
				CreateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
			},
			want: []string{"session_id", "title", "trace_id", "is_manual", "update_time", "create_time"},
		},
		{
			name: "Message",
			target: Message{
				ID: 42, SessionID: "s-1", Agent: AgentConfucius,
				MsgIndex: 3, Role: RoleUser, EventType: EventTypeMessage,
				Content: "hi", TraceID: "t-1", RunID: "call_x1", AgentInstanceID: "w-abc123",
				MsgTime:    time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
				UpdateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
			},
			want: []string{"id", "session_id", "msg_time", "agent", "msg_index",
				"role", "event_type", "content", "trace_id", "client_msg_id", "run_id",
				"agent_instance_id", "update_time"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.target)
			if err != nil {
				t.Fatalf("marshal %s: %v", tc.name, err)
			}
			body := string(raw)

			for _, key := range tc.want {
				needle := `"` + key + `"`
				if !strings.Contains(body, needle) {
					t.Errorf("%s JSON missing key %q; body=%s", tc.name, key, body)
				}
			}
			for _, key := range tc.skip {
				needle := `"` + key + `"`
				if strings.Contains(body, needle) {
					t.Errorf("%s JSON must not emit key %q; body=%s", tc.name, key, body)
				}
			}
		})
	}
}

func TestMessage_InstanceIDOmitEmpty(t *testing.T) {
	legacy, err := json.Marshal(Message{ID: 1, SessionID: "s", Agent: AgentConfucius})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "agent_instance_id") {
		t.Fatalf("legacy payload must omit agent_instance_id: %s", legacy)
	}
	stamped, err := json.Marshal(Message{ID: 1, AgentInstanceID: "w-abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stamped), `"agent_instance_id":"w-abc123"`) {
		t.Fatalf("stamped payload must carry agent_instance_id: %s", stamped)
	}
}

func TestConstants_RolesAndAgents(t *testing.T) {
	roles := []string{RoleUser, RoleAssistant, RoleTool}
	for _, r := range roles {
		if r == "" {
			t.Fatal("role constant must be non-empty")
		}
	}
	agents := []string{AgentUser, AgentConfucius, AgentChongzhi, AgentLiang}
	for _, a := range agents {
		if a == "" {
			t.Fatal("agent constant must be non-empty")
		}
	}
	eventTypes := []string{
		EventTypeMessage, EventTypeToken, EventTypeToolCall,
		EventTypeAgentStart, EventTypeAgentEnd, EventTypeAgentError,
	}
	for _, e := range eventTypes {
		if e == "" {
			t.Fatal("event type constant must be non-empty")
		}
	}
}
