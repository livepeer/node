package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

func TestRegressionSubmittedTransactionHashSurvivesWaitFailure(t *testing.T) {
	for _, failure := range []string{"reverted", "pending", "rpc-error"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			path := filepath.Join(t.TempDir(), "key")
			require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("0", 63)+"1"), 0600))
			key, err := eth.OpenKeyFile(path)
			require.NoError(t, err)
			hash := "0x" + strings.Repeat("1", 64)
			rpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				var result any = "0x1"
				switch req.Method {
				case "eth_chainId", "eth_gasPrice", "eth_getTransactionCount":
				case "eth_call":
					result = "0x" + strings.Repeat("0", 60) + "2000"
				case "eth_estimateGas":
					result = "0x5208"
				case "eth_sendRawTransaction":
					var encoded string
					require.NoError(t, json.Unmarshal(req.Params[0], &encoded))
					raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "0x"))
					require.NoError(t, err)
					var tx types.Transaction
					require.NoError(t, tx.UnmarshalBinary(raw))
					hash = tx.Hash().Hex()
					result = hash
				case "eth_getTransactionReceipt":
					switch failure {
					case "reverted":
						result = map[string]string{"status": "0x0", "blockNumber": "0x10"}
					case "pending":
						cancel()
						result = nil
					case "rpc-error":
						http.Error(w, "receipt unavailable", http.StatusServiceUnavailable)
						return
					}
				default:
					t.Errorf("unexpected RPC method %s", req.Method)
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
			}))
			defer rpcServer.Close()
			params := Params{RPCURL: rpcServer.URL, RPCGrants: []string{strings.TrimPrefix(rpcServer.URL, "http://")}, ChainID: "1", Sender: key.Address().Hex(), Controller: "0x0000000000000000000000000000000000001000", KeyFile: path, Submit: true, Wait: true, Output: "json"}
			var output bytes.Buffer
			err = executeAction(ctx, params, &output, "ticketbroker unlock")
			require.Error(t, err)
			require.Contains(t, output.String()+err.Error(), hash, "the submitted transaction must remain identifiable after its receipt fails")
		})
	}
}
