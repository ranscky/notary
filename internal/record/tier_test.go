package record_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

func TestVisibilityTier(t *testing.T) {
	assert.True(t, record.Observed.Valid())
	assert.True(t, record.Reconstructed.Valid())
	assert.True(t, record.Internal.Valid())

	assert.False(t, record.VisibilityTier{}.Valid(), "the zero value must be invalid")

	assert.Equal(t, "observed", record.Observed.String())
	assert.Equal(t, "reconstructed", record.Reconstructed.String())
	assert.Equal(t, "internal", record.Internal.String())

	assert.NotEqual(t, record.Observed, record.Reconstructed)
	assert.NotEqual(t, record.Observed, record.Internal)
	assert.NotEqual(t, record.Reconstructed, record.Internal)

	b, err := json.Marshal(record.Observed)
	require.NoError(t, err)
	assert.Equal(t, []byte(`"observed"`), b)

	var rt record.VisibilityTier
	require.NoError(t, json.Unmarshal([]byte(`"reconstructed"`), &rt))
	assert.Equal(t, record.Reconstructed, rt)
}

func TestVisibilityTierUnmarshalRejectsUnknown(t *testing.T) {
	t.Run("unknown string leaves receiver invalid", func(t *testing.T) {
		tt := record.Observed
		err := json.Unmarshal([]byte(`"bogus"`), &tt)
		require.Error(t, err)
		assert.False(t, tt.Valid())
	})

	t.Run("empty string leaves receiver invalid", func(t *testing.T) {
		var tt record.VisibilityTier
		err := json.Unmarshal([]byte(`""`), &tt)
		require.Error(t, err)
		assert.False(t, tt.Valid())
	})
}
