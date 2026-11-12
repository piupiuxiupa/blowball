package rpcx

import (
	"errors"
	"fmt"
)

// SecurityMode binds internal RPC transport to its caller authentication.
type SecurityMode string

const (
	SecurityModeMTLS           SecurityMode = "mtls"
	SecurityModePlaintextToken SecurityMode = "plaintext_token"
)

// NormalizeSecurityMode validates the mode and applies the secure default.
func NormalizeSecurityMode(mode SecurityMode) (SecurityMode, error) {
	switch mode {
	case "", SecurityModeMTLS:
		return SecurityModeMTLS, nil
	case SecurityModePlaintextToken:
		return SecurityModePlaintextToken, nil
	default:
		return "", errors.New("rpcx: unsupported security_mode (expected mtls or plaintext_token)")
	}
}

// ValidatePeerTokens checks unambiguous bindings to known service identities.
func ValidatePeerTokens(tokens map[string]string) error {
	if len(tokens) == 0 {
		return errors.New("rpcx: peer_tokens is required")
	}
	seen := make(map[string]struct{}, len(tokens))
	for identity, token := range tokens {
		switch identity {
		case IdentityScheduler, IdentityAPI, IdentityAgent, IdentityMain:
		default:
			return errors.New("rpcx: peer_tokens contains an unsupported service identity")
		}
		if err := ValidateClientToken(token); err != nil {
			return fmt.Errorf("rpcx: invalid peer_tokens value: %w", err)
		}
		if _, ok := seen[token]; ok {
			return errors.New("rpcx: peer_tokens cannot share a token between service identities")
		}
		seen[token] = struct{}{}
	}
	return nil
}

// ValidateClientToken checks that a token is one nonempty ASCII credential.
func ValidateClientToken(token string) error {
	if token == "" {
		return errors.New("rpcx: token is required")
	}
	for i := range len(token) {
		if token[i] < '!' || token[i] > '~' {
			return errors.New("rpcx: token must be printable ASCII without whitespace")
		}
	}
	return nil
}
