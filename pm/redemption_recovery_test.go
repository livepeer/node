package pm

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

func TestRedemptionRecoveryBeforeAndAfterBroadcast(t *testing.T) {
	for _, failure := range []string{"preparation", "prepared-crash", "uncertain-broadcast"} {
		t.Run(failure, func(t *testing.T) {
			keyFile := filepath.Join(t.TempDir(), "key")
			require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("0", 63)+"1"), 0600))
			key, err := eth.OpenKeyFile(keyFile)
			require.NoError(t, err)
			path := filepath.Join(t.TempDir(), "payments.sqlite")
			store, err := OpenSQLite(path)
			require.NoError(t, err)
			defer func() { _ = store.Close() }()
			ticket := &SignedTicket{Ticket: &Ticket{Sender: ethcommon.HexToAddress("0x1234"), Recipient: key.Address(), FaceValue: big.NewInt(10), WinProb: big.NewInt(100), SenderNonce: 1, RecipientRandHash: ethcommon.HexToHash("0xabcd"), CreationRound: 5, ParamsExpirationBlock: big.NewInt(10)}, Sig: []byte{1, 2, 3}, RecipientRand: big.NewInt(4)}
			require.NoError(t, store.StoreWinningTicket(ticket))
			var fail atomic.Bool
			fail.Store(true)
			var broadcasts atomic.Int32
			var firstRaw string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				result := "0x1"
				switch req.Method {
				case "eth_call":
					result = "0x" + strings.Repeat("0", 60) + "2000"
				case "eth_estimateGas":
					if failure == "preparation" && fail.Load() {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					result = "0x5208"
				case "eth_gasPrice", "eth_getTransactionCount":
				case "eth_sendRawTransaction":
					broadcasts.Add(1)
					var encoded string
					require.NoError(t, json.Unmarshal(req.Params[0], &encoded))
					if firstRaw != "" {
						require.Equal(t, firstRaw, encoded, "recovery must reuse the exact transaction")
					}
					firstRaw = encoded
					if failure == "uncertain-broadcast" && fail.Load() {
						http.Error(w, "lost response", http.StatusServiceUnavailable)
						return
					}
					raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "0x"))
					require.NoError(t, err)
					var transaction types.Transaction
					require.NoError(t, transaction.UnmarshalBinary(raw))
					result = transaction.Hash().Hex()
				default:
					t.Errorf("unexpected RPC %s", req.Method)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			}))
			defer server.Close()
			rpc, err := eth.OpenRPC(server.URL, []string{strings.TrimPrefix(server.URL, "http://")}, "")
			require.NoError(t, err)
			contracts, err := eth.OpenContracts(rpc, ethcommon.HexToAddress("0x1000").Hex())
			require.NoError(t, err)
			chain := eth.PaymentChain{Contracts: contracts}
			snapshot := eth.ChainSnapshot{Block: big.NewInt(10), Round: big.NewInt(5)}
			if failure == "prepared-crash" {
				prepared, err := chain.PrepareRedemption(t.Context(), key, big.NewInt(1), eth.RedeemTicket{Recipient: ticket.Recipient, Sender: ticket.Sender, FaceValue: ticket.FaceValue, WinProb: ticket.WinProb, SenderNonce: ticket.SenderNonce, RecipientRandHash: ticket.RecipientRandHash, AuxData: ticket.AuxData(), Signature: ticket.Sig, RecipientRand: ticket.RecipientRand})
				require.NoError(t, err)
				require.NoError(t, store.recordPrepared(t.Context(), ticket, prepared))
			} else {
				require.NotEmpty(t, RedeemPending(t.Context(), store, chain, key, big.NewInt(1), snapshot))
			}
			require.NoError(t, store.Close())
			store, err = OpenSQLite(path)
			require.NoError(t, err)
			fail.Store(false)
			require.Empty(t, RedeemPending(t.Context(), store, chain, key, big.NewInt(1), snapshot))
			require.Equal(t, int32(1), broadcasts.Load(), "uncertain sends cannot retry automatically")
			items, err := store.Redemptions()
			require.NoError(t, err)
			require.Len(t, items, 1)
			if failure == "uncertain-broadcast" {
				require.Equal(t, "broadcast", items[0].Phase)
				require.NoError(t, RetryRedemption(t.Context(), store, contracts, ethcommon.HexToHash(items[0].Hash)))
				require.Equal(t, int32(2), broadcasts.Load())
			}
			require.Empty(t, ReconcileSubmitted(t.Context(), store, receiptFixture{confirmed: true}))
			require.Error(t, RetryRedemption(t.Context(), store, contracts, ethcommon.HexToHash(items[0].Hash)))
		})
	}
}
