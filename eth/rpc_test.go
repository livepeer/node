package eth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRPCResponseBodyIsLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxRPCResponse+1))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	rpc, err := OpenRPC(server.URL, []string{parsed.Host}, "")
	require.NoError(t, err)
	_, err = rpc.Call(t.Context(), "eth_chainId")
	require.ErrorContains(t, err, "exceeds 1 MiB")
}
