package pm

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

// Adapted from go-livepeer/ai/runner/live_runner_test.go's hour/fixed conversion cases.
func TestConvertRunnerPrice(t *testing.T) {
	rate := big.NewRat(7200, 1)
	perSecond, unit, err := ConvertRunnerPrice("0.5", "hour", rate)
	require.NoError(t, err)
	require.Equal(t, "seconds", unit)
	require.Equal(t, "1", perSecond.String())
	fixed, unit, err := ConvertRunnerPrice("0.5", "fixed", rate)
	require.NoError(t, err)
	require.Equal(t, "fixed", unit)
	require.Equal(t, "3600", fixed.String())
	_, _, err = ConvertRunnerPrice("1", "720p", rate)
	require.Error(t, err)
}
