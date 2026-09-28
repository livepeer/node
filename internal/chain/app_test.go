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

func TestAccountValidatesSenderAndReadsPendingNonce(t *testing.T) {
	const sender = "0x0123456789abcdef0123456789abcdef01234567"
	var seen []string
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		seen = append(seen, request.Method)
		value := "0x1"
		switch request.Method {
		case "eth_getBalance":
			require.Equal(t, []any{sender, "latest"}, request.Params)
			value = "0xde0b6b3a7640000"
		case "eth_getTransactionCount":
			require.Equal(t, []any{sender, "pending"}, request.Params)
			value = "0x2"
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + value + `"}`))
	}))
	defer rpc.Close()
	params := Params{RPCURL: rpc.URL, RPCGrants: []string{strings.TrimPrefix(rpc.URL, "http://")}, ChainID: "1", Sender: sender, Output: "json"}
	var out bytes.Buffer
	require.NoError(t, Account(t.Context(), params, &out))
	require.JSONEq(t, `{"address":"`+sender+`","balance_wei":"1000000000000000000","nonce":2}`, out.String())
	require.Equal(t, []string{"eth_chainId", "eth_getBalance", "eth_getTransactionCount"}, seen)
	params.Sender = "0xinvalid"
	require.ErrorContains(t, Account(t.Context(), params, &out), "sender")
	require.Len(t, seen, 3)
}

func TestRPCResponseBodyIsLimited(t *testing.T) {
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), (1<<20)+1))
	}))
	defer rpc.Close()
	_, err := call(t.Context(), http.DefaultClient, rpc.URL, "eth_chainId")
	require.ErrorContains(t, err, "exceeds 1 MiB")
}
