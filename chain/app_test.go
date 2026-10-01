package chain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func pointer[T any](value T) *T { return &value }

func testURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func rpcParams(t *testing.T, endpoint string) OperatorParams {
	t.Helper()
	return OperatorParams{
		RPCURL:     testURL(t, endpoint),
		Controller: ethcommon.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4"),
	}
}

func TestStatusChecksChainID(t *testing.T) {
	var seen []string
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		seen = append(seen, request.Method)
		switch request.Method {
		case "eth_chainId":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
		case "eth_blockNumber":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`))
		default:
			t.Errorf("unexpected RPC method %s", request.Method)
		}
	}))
	defer rpc.Close()
	params := rpcParams(t, rpc.URL)
	var out bytes.Buffer
	for _, mode := range []string{"json", "text"} {
		seen = nil
		out.Reset()
		require.NoError(t, Status(t.Context(), params, DisplayOptions{Output: mode}, &out))
		if mode == "json" {
			require.JSONEq(t, `{"chain_id":"1","block_number":"0x10"}`, out.String())
		} else {
			require.Equal(t, "Chain ID: 1\nBlock: 0x10\n", out.String())
		}
		require.Equal(t, []string{"eth_chainId", "eth_blockNumber"}, seen)
	}
	params.ChainID = pointer(uint64(2))
	out.Reset()
	seen = nil
	require.ErrorContains(t, Status(t.Context(), params, DisplayOptions{Output: "json"}, &out), "does not match")
	require.Empty(t, out.String())
	require.Equal(t, []string{"eth_chainId"}, seen, "a mismatched chain must stop the command")
}

func TestAccountValidatesSenderAndReadsPendingNonce(t *testing.T) {
	sender := ethcommon.HexToAddress("0x0123456789abcdef0123456789abcdef01234567")
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
		case "eth_chainId":
		case "eth_getBalance":
			require.Equal(t, []any{sender.Hex(), "latest"}, request.Params)
			value = "0xde0b6b3a7640000"
		case "eth_getTransactionCount":
			require.Equal(t, []any{sender.Hex(), "pending"}, request.Params)
			value = "0x2"
		default:
			t.Errorf("unexpected RPC method %s", request.Method)
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + value + `"}`))
	}))
	defer rpc.Close()
	params := rpcParams(t, rpc.URL)
	params.Sender = &sender
	var out bytes.Buffer
	for _, mode := range []string{"json", "text"} {
		seen = nil
		out.Reset()
		require.NoError(t, Account(t.Context(), params, DisplayOptions{Output: mode}, &out))
		if mode == "json" {
			require.JSONEq(t, `{"address":"`+sender.Hex()+`","balance_wei":"1000000000000000000","nonce":2}`, out.String())
		} else {
			require.Equal(t, "Address: "+sender.Hex()+"\nETH balance (wei): 1000000000000000000\nPending nonce: 2\n", out.String())
		}
		require.Equal(t, []string{"eth_chainId", "eth_getBalance", "eth_getTransactionCount"}, seen)
	}
	params.Sender = nil
	require.ErrorContains(t, Account(t.Context(), params, DisplayOptions{Output: "json"}, &out), "sender")
	require.Len(t, seen, 3)
}

func TestPrintConfigOmitsCredentials(t *testing.T) {
	dir := t.TempDir()
	rpcFile := filepath.Join(dir, "rpc")
	keyFile := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(rpcFile, []byte("http://localhost:8545?key=private-token"), 0600))
	require.NoError(t, os.WriteFile(keyFile, []byte("private-key"), 0600))
	configFile := filepath.Join(dir, "chain.toml")
	config := fmt.Sprintf(`RPCURLFile = %q
KeyFile = %q
ChainID = 42161
`, rpcFile, keyFile)
	require.NoError(t, os.WriteFile(configFile, []byte(config), 0600))
	for _, command := range [][]string{nil, {"status"}, {"stake", "bond"}, {"orchestrator", "activate"}, {"ticketbroker", "fund"}} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(append(command, "--config", configFile, "--print-config", "--output", "json"))
			require.NoError(t, root.Execute())
			for _, secret := range []string{"private-token", rpcFile, keyFile, configFile} {
				require.NotContains(t, output.String(), secret)
			}
			var printed map[string]any
			require.NoError(t, toml.Unmarshal(output.Bytes(), &printed))
			require.EqualValues(t, 42161, printed["ChainID"])
			for _, key := range []string{"RPCURL", "RPCURLFile", "KeyFile", "ConfigFile", "Amount", "Reserve", "ServiceURI", "Output", "PrintConfig", "Submit", "Wait", "Quiet"} {
				require.NotContains(t, printed, key)
			}
		})
	}
}
