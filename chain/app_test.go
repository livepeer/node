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

func TestPrintConfigOmitsCredentials(t *testing.T) {
	dir := t.TempDir()
	rpcFile := filepath.Join(dir, "rpc")
	keyFile, passwordPath := "/missing/account.json", filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(passwordPath, []byte("keystore-secret"), 0600))
	require.NoError(t, os.WriteFile(rpcFile, []byte("http://localhost:8545?key=private-token"), 0600))
	configFile := filepath.Join(dir, "chain.toml")
	config := fmt.Sprintf(`RPCURLFile = %q
KeystoreFile = %q
KeystorePasswordFile = %q
ChainID = 42161
`, rpcFile, keyFile, passwordPath)
	require.NoError(t, os.WriteFile(configFile, []byte(config), 0600))
	for _, command := range [][]string{nil, {"status"}, {"account", "create"}, {"account", "get"}, {"orchestrator"}, {"stake", "locks", "--withdrawable", "--locked"}, {"stake", "bond"}, {"orchestrator", "register"}, {"ticketbroker", "fund"}, {"sign", "message"}, {"sign", "typed-data"}} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(append(command, "--config", configFile, "--print-config", "--output", "json"))
			require.NoError(t, root.Execute())
			for _, secret := range []string{"private-token", "keystore-secret", rpcFile, keyFile, passwordPath, configFile} {
				require.NotContains(t, output.String(), secret)
			}
			var printed map[string]any
			require.NoError(t, toml.Unmarshal(output.Bytes(), &printed))
			require.EqualValues(t, 42161, printed["ChainID"])
			for _, key := range []string{"RPCURL", "RPCURLFile", "KeystoreFile", "KeystorePassword", "KeystorePasswordFile", "ConfigFile", "Amount", "Reserve", "ServiceURI", "Output", "PrintConfig", "Submit", "Wait", "Quiet"} {
				require.NotContains(t, printed, key)
			}
		})
	}
}
