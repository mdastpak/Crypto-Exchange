package region

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	r, err := Parse(" IR ")
	require.NoError(t, err)
	assert.True(t, r.Iran())
	assert.False(t, r.International())

	r, err = Parse("international")
	require.NoError(t, err)
	assert.True(t, r.International())
	assert.True(t, r.In(Iran, International))
	assert.False(t, r.In(Iran))
	assert.Equal(t, "international", r.String())

	for _, bad := range []string{"", "global", "iran", "eu"} {
		_, err := Parse(bad)
		assert.Error(t, err, bad)
	}
	assert.Equal(t, Iran, Default)
}
