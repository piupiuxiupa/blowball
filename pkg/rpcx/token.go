package rpcx

import (
	"context"
	"crypto/subtle"
	"strings"

	"google.golang.org/grpc/metadata"
)

// tokenCredentials is only installed by the explicit plaintext profile.
type tokenCredentials struct {
	token string
}

func (c tokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + c.token}, nil
}

func (tokenCredentials) RequireTransportSecurity() bool {
	return false
}

func tokenIdentity(ctx context.Context, peers map[string]string) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	values := md.Get("authorization")
	if len(values) != 1 {
		return "", false
	}
	var scheme, token string
	count := 0
	for part := range strings.FieldsSeq(values[0]) {
		switch count {
		case 0:
			scheme = part
		case 1:
			token = part
		default:
			return "", false
		}
		count++
	}
	if count != 2 || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	var identity string
	for peer, expected := range peers {
		if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
			identity = peer
		}
	}
	return identity, identity != ""
}
