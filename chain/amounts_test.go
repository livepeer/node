package chain

import (
	"github.com/stretchr/testify/require"
	"math/big"
	"testing"
)

func TestExactAmountConversion(t *testing.T) {
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()
	for _, tc := range []struct {
		raw             string
		base, all       bool
		expected, error string
	}{
		{"1", false, false, "1000000000000000000", ""}, {"0.000000000000000001", false, false, "1", ""}, {"1.234567890123456789", false, false, "1234567890123456789", ""}, {"5", true, false, "5", ""},
		{"all", false, true, "", ""}, {"all", true, false, "", "not supported"}, {"0", false, false, "", "positive"}, {"1.0000000000000000001", false, false, "", "18 decimal"}, {"1.1", true, false, "", "0 decimal"}, {"-1", false, false, "", "unsigned"}, {"1e2", false, false, "", "unsigned"}, {"NaN", false, false, "", "unsigned"}, {max, true, false, max, ""}, {max, false, false, "", "uint256"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			value, err := parseAmount(tc.raw, tc.base, tc.all, true)
			if tc.error != "" {
				require.ErrorContains(t, err, tc.error)
			} else {
				require.NoError(t, err)
				if tc.raw == "all" {
					require.Nil(t, value)
				} else {
					require.Equal(t, tc.expected, value.String())
				}
			}
		})
	}
	for _, raw := range []string{"0", "1.2345", "100"} {
		value, err := parsePercent(raw)
		require.NoError(t, err)
		expected := map[string]string{"0": "0", "1.2345": "12345", "100": "1000000"}
		require.Equal(t, expected[raw], value.String())
	}
	for _, raw := range []string{"100.0001", "1.00001", "-1"} {
		_, err := parsePercent(raw)
		require.Error(t, err)
	}
}
