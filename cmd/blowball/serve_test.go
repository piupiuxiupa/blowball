package main

import (
	"testing"
)

// TestValidateRole_ValidatesValues exercises --role validation: accepted
// values pass; an unknown value returns an error (surfaced as a non-zero exit
// before any setup runs). The role-aware key requirement and route-group
// naming tests live in pkg/boot, next to the functions they cover.
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
