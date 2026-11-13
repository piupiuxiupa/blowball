package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalAttachmentRefs_EmptyIsNil(t *testing.T) {
	assert.Nil(t, MarshalAttachmentRefs(nil))
	assert.Nil(t, MarshalAttachmentRefs([]MessageAttachmentRef{}))
}

func TestMarshalAttachmentRefs_RoundTrip(t *testing.T) {
	refs := []MessageAttachmentRef{
		{ID: "01953c5e-7f9e-7cc8-8db5-2f4a5b6c7d8e", Filename: "截图.png", MIME: "image/png"},
		{ID: "01953c5e-8a11-7c22-9f00-1b2c3d4e5f60", Filename: "photo.jpg", MIME: "image/jpeg"},
	}
	b := MarshalAttachmentRefs(refs)
	require.NotNil(t, b)
	assert.Equal(t, refs, UnmarshalAttachmentRefs(b))
}

func TestUnmarshalAttachmentRefs_MalformedIsNil(t *testing.T) {
	assert.Nil(t, UnmarshalAttachmentRefs(nil))
	assert.Nil(t, UnmarshalAttachmentRefs([]byte{}))
	assert.Nil(t, UnmarshalAttachmentRefs([]byte("{not json")))
}
