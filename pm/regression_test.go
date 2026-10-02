package pm

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

func TestRegressionRedemptionWaitsForParameterExpiry(t *testing.T) {
	for _, block := range []int64{9, 10, 11} {
		t.Run(big.NewInt(block).String(), func(t *testing.T) {
			path, passwordPath := test.WriteFixedKeystore(t)
			key, err := eth.OpenKeystoreFile(path, passwordPath)
			require.NoError(t, err)
			store, err := OpenSQLite(filepath.Join(t.TempDir(), "payment.sqlite"))
			require.NoError(t, err)
			defer store.Close()
			ticket := &SignedTicket{Ticket: &Ticket{Sender: ethcommon.HexToAddress("0x1234"), Recipient: key.Address(), FaceValue: big.NewInt(10), WinProb: big.NewInt(100), SenderNonce: 1, RecipientRandHash: ethcommon.HexToHash("0xabcd"), CreationRound: 5, ParamsExpirationBlock: big.NewInt(10)}, Sig: []byte{1, 2, 3}, RecipientRand: big.NewInt(4)}
			require.NoError(t, store.StoreWinningTicket(ticket))
			var broadcasts atomic.Int32
			rpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				var result any = "0x"
				switch req.Method {
				case "eth_call":
					result = "0x" + strings.Repeat("0", 60) + "2000"
				case "eth_getBlockByNumber":
					result = &types.Header{Number: big.NewInt(1), Difficulty: new(big.Int), BaseFee: big.NewInt(1)}
				case "eth_estimateGas":
					result = "0x5208"
				case "eth_maxPriorityFeePerGas":
					result = "0x1"
				case "eth_getTransactionCount":
					result = "0x1"
				case "eth_sendRawTransaction":
					broadcasts.Add(1)
					var encoded string
					require.NoError(t, json.Unmarshal(req.Params[0], &encoded))
					raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "0x"))
					require.NoError(t, err)
					var tx types.Transaction
					require.NoError(t, tx.UnmarshalBinary(raw))
					result = tx.Hash().Hex()
					// The exact transaction identity must exist before sending.
					items, err := store.Redemptions()
					require.NoError(t, err)
					require.Equal(t, []RedemptionStatus{{Hash: result.(string), Phase: "broadcast"}}, items)
				default:
					t.Errorf("unexpected RPC method %s", req.Method)
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
			}))
			defer rpcServer.Close()
			rpc, err := eth.OpenRPC(rpcServer.URL)
			require.NoError(t, err)
			contracts, err := eth.OpenContracts(rpc, "0x0000000000000000000000000000000000001000")
			require.NoError(t, err)
			failures := RedeemPending(t.Context(), store, eth.PaymentChain{Contracts: contracts}, key, big.NewInt(1), eth.ChainSnapshot{Block: big.NewInt(block), Round: big.NewInt(5)})
			require.Empty(t, failures)
			expected := int32(0)
			if block >= 10 {
				expected = 1
			}
			require.Equal(t, expected, broadcasts.Load(), "parameter expiry gates safe redemption; block=%d failures=%v", block, failures)
		})
	}
}
