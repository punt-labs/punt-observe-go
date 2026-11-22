package telemetry

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallationSaltConvergesAndSeparatesHomes(t *testing.T) {
	home := freshHome(t)
	var values [12][]byte
	var errs [12]error
	var wg sync.WaitGroup
	for i := range values {
		wg.Go(func() { values[i], errs[i] = installationSalt(t.Context()) })
	}
	wg.Wait()
	for i, value := range values {
		require.NoError(t, errs[i])
		assert.Len(t, value, saltBytes)
		assert.Equal(t, values[0], value)
	}
	assertMode(t, filepath.Join(home, ".punt-labs", saltName), 0o600)
	freshHome(t)
	other, err := installationSalt(t.Context())
	require.NoError(t, err)
	assert.NotEqual(t, values[0], other)
	field := Field{Kind: Identifier}
	input := HashIdentifier("subject", "fixture-identifier")
	x, accepted := typedAttribute(field, input, values[0])
	require.True(t, accepted)
	y, accepted := typedAttribute(field, input, other)
	require.True(t, accepted)
	assert.NotEqual(t, x, y)
}
