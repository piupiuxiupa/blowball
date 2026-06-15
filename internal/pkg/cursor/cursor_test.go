package cursor

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	want := Cursor{
		MsgTime:  time.Unix(1_700_000_000, 123_456_789).UTC(),
		MsgIndex: 7,
		ID:       42,
	}
	token, err := Encode(want)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	got, err := Decode(token)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestEncode_EmptyCursor_ReturnsEmpty(t *testing.T) {
	token, err := Encode(Cursor{})
	require.NoError(t, err)
	assert.Empty(t, token)
}

func TestDecode_EmptyToken_ReturnsEmpty(t *testing.T) {
	c, err := Decode("")
	require.NoError(t, err)
	assert.Equal(t, Cursor{}, c)
}

func TestDecode_InvalidBase64_ReturnsError(t *testing.T) {
	_, err := Decode("!!!")
	require.Error(t, err)
}

func TestDecode_InvalidJSON_ReturnsError(t *testing.T) {
	// Valid base64 of ungzipped JSON.
	_, err := Decode("eyJmb28iOiJiYXIifQ==")
	require.Error(t, err)
}
