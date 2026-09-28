package chain

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatusChecksChainID(t *testing.T) {
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		if request.Method == "eth_chainId" {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`))
	}))
	defer rpc.Close()
	params := Params{RPCURL: rpc.URL, RPCGrants: []string{strings.TrimPrefix(rpc.URL, "http://")}, ChainID: "1", Output: "json"}
	var out bytes.Buffer
	require.NoError(t, Status(t.Context(), params, &out))
	require.JSONEq(t, `{"chain_id":"1","block_number":"0x10"}`, out.String())
	params.ChainID = "2"
	require.ErrorContains(t, Status(t.Context(), params, &out), "does not match")
}
