package pm

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

// Adapted from go-livepeer/ai/runner/live_runner_test.go's hour/fixed conversion cases.
func TestConvertRunnerPrice(t *testing.T) {
	for _, tt := range []struct{ price, unit, wei, convertedUnit string }{
		{"0.5", "hour", "1", "seconds"},
		{"0.5", "fixed", "3600", "fixed"},
		{"0.00001", "hour", "1", "seconds"},
		{"0.0002", "fixed", "2", "fixed"},
	} {
		t.Run(tt.price+"/"+tt.unit, func(t *testing.T) {
			wei, unit, err := ConvertRunnerPrice(tt.price, tt.unit, big.NewRat(7200, 1))
			require.NoError(t, err)
			require.Equal(t, tt.wei, wei.String())
			require.Equal(t, tt.convertedUnit, unit)
		})
	}
}
