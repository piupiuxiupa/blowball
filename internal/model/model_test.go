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
		"update_time", "create_time",
	},
	"Title": {
		"session_id", "title", "trace_id",
		"update_time", "create_time",
	},
	"Message": {
		"id", "session_id", "msg_time", "agent", "msg_index",
		"role", "content", "trace_id", "update_time",
	},
}

// structTypes returns the model structs covered by the test.
func structTypes() map[string]interface{} {
	return map[string]interface{}{
		"User":    User{},
		"Session": Session{},
		"Title":   Title{},
		"Message": Message{},
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
		target interface{}
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
			want: []string{"session_id", "user_id", "trace_id", "update_time", "create_time"},
		},
		{
			name: "Title",
			target: Title{
				SessionID: "s-1", Title: "hello", TraceID: "t-1",
				UpdateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
				CreateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
			},
			want: []string{"session_id", "title", "trace_id", "update_time", "create_time"},
		},
		{
			name: "Message",
			target: Message{
				ID: 42, SessionID: "s-1", Agent: AgentConfuse,
				MsgIndex: 3, Role: RoleUser, Content: "hi", TraceID: "t-1",
				MsgTime:    time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
				UpdateTime: time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC),
			},
			want: []string{"id", "session_id", "msg_time", "agent", "msg_index",
				"role", "content", "trace_id", "update_time"},
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

func TestConstants_RolesAndAgents(t *testing.T) {
	roles := []string{RoleUser, RoleAssistant, RoleTool}
	for _, r := range roles {
		if r == "" {
			t.Fatal("role constant must be non-empty")
		}
	}
	agents := []string{AgentConfuse, AgentChongzhi, AgentLiang}
	for _, a := range agents {
		if a == "" {
			t.Fatal("agent constant must be non-empty")
		}
	}
}
