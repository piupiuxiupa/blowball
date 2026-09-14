package main

import (
	"testing"
)

// TestOpenAIKeyRequired verifies the role-aware openai.api_key requirement.
// Only the api role is exempt: it never constructs the OpenAI client or
// orchestrator, so it must not be gated on an LLM key at startup. The all and
// agent roles build the agent layer and therefore require the key. This is the
// decision-level enforcement of the service-roles spec's fault-isolation
// requirement that "the api role does not depend on the agent layer".
func TestOpenAIKeyRequired(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{"api", false},
		{"all", true},
		{"agent", true},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			if got := openAIKeyRequired(tc.role); got != tc.want {
				t.Errorf("openAIKeyRequired(%q) = %v, want %v", tc.role, got, tc.want)
			}
		})
	}
}

// TestRouteGroups verifies the startup-log route partition names per role.
func TestRouteGroups(t *testing.T) {
	cases := []struct {
		role string
		want []string
	}{
		{"api", []string{"api"}},
		{"agent", []string{"agent"}},
		{"all", []string{"api", "agent"}},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			got := routeGroups(tc.role)
			if len(got) != len(tc.want) {
				t.Fatalf("routeGroups(%q) = %v, want %v", tc.role, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("routeGroups(%q)[%d] = %q, want %q", tc.role, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestValidateRole_ValidatesValues exercises --role validation: accepted
// values pass; an unknown value returns an error (surfaced as a non-zero exit
// before any setup runs).
func TestValidateRole_ValidatesValues(t *testing.T) {
	cases := []struct {
		role    string
		wantErr bool
	}{
		{"all", false},
		{"api", false},
		{"agent", false},
		{"foo", true},
		{"", true}, // empty is not one of the valid roles
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			err := validateRole(tc.role)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateRole(%q) err = %v, wantErr %v", tc.role, err, tc.wantErr)
			}
		})
	}
}
