package artifact

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEscapePath(t *testing.T) {
	assert.Equal(t, "reports/a%20b.docx", escapePath("reports/a b.docx"))
	assert.Equal(t, "%E8%B4%A2%E5%8A%A1/2024/%E5%B9%B4%E6%8A%A5.docx", escapePath("财务/2024/年报.docx"))
	assert.Equal(t, "a/b/c", escapePath("a/b/c"))   // separators preserved
	assert.Equal(t, "a+b", escapePath("a+b"))       // plus passes through (same as workspace escapeOnlyOfficePath)
	assert.Equal(t, "a%252Fb", escapePath("a%2Fb")) // pre-escaped input double-escapes, never collapses
}
