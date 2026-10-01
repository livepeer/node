package signer

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHeadersSyntaxAndRoundTrip(t *testing.T) {
	var headers Headers
	require.NoError(t, headers.UnmarshalText([]byte(`authorization: Bearer token, X-Tag: one, x-tag: two, "X-List: a,b", "X-Quote: say ""hi"""`)))
	require.Equal(t, "Bearer token", http.Header(headers).Get("Authorization"))
	require.Equal(t, []string{"one", "two"}, http.Header(headers).Values("X-Tag"))
	require.Equal(t, "a,b", http.Header(headers).Get("X-List"))
	encoded, err := headers.MarshalText()
	require.NoError(t, err)
	var decoded Headers
	require.NoError(t, decoded.UnmarshalText(encoded))
	require.Equal(t, headers, decoded)
	second, err := headers.MarshalText()
	require.NoError(t, err)
	require.Equal(t, encoded, second)
	for _, input := range []string{"missing colon", ": value", "Bad Name: value", "X: value\x00", "X: value\nInjected: bad", `"X: value`} {
		t.Run(input, func(t *testing.T) {
			require.Error(t, decoded.UnmarshalText([]byte(input)))
			require.Equal(t, headers, decoded, "a parse failure must not partially replace the headers")
		})
	}
}
