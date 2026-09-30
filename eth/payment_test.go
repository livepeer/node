package eth

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

func TestPaymentSnapshotUsesCanonicalL1AndInitializedRound(t *testing.T) {
	for _, l2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "ethereum", true: "arbitrum"}[l2], func(t *testing.T) {
			blockHash := ethcommon.HexToHash("0xabcd").Hex()
			var reorg atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				var result any
				switch request.Method {
				case "eth_getBlockByNumber":
					header := map[string]string{"number": "0x100000", "hash": blockHash}
					if l2 {
						header["l1BlockNumber"] = "0x32"
					}
					result = header
				case "eth_call":
					var ref map[string]any
					require.NoError(t, json.Unmarshal(request.Params[1], &ref))
					require.Equal(t, map[string]any{"blockHash": blockHash, "requireCanonical": true}, ref)
					if reorg.Load() {
						_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32000}})
						return
					}
					var call map[string]string
					require.NoError(t, json.Unmarshal(request.Params[0], &call))
					selector := func(sig string) bool {
						return strings.HasPrefix(call["data"], "0x"+hex.EncodeToString(crypto.Keccak256([]byte(sig))[:4]))
					}
					switch {
					case selector("getContract(bytes32)"):
						result = "0x" + hex.EncodeToString(ethcommon.LeftPadBytes([]byte{1}, 32))
					case selector("lastInitializedRound()"):
						result = "0x" + hex.EncodeToString(ethcommon.LeftPadBytes([]byte{5}, 32))
					case selector("blockHashForRound(uint256)"):
						require.True(t, strings.HasSuffix(call["data"], strings.Repeat("0", 63)+"5"))
						result = blockHash
					default:
						t.Errorf("unexpected call %s", call["data"])
					}
				default:
					t.Errorf("unexpected RPC %s", request.Method)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			}))
			defer server.Close()
			rpc, err := OpenRPC(server.URL, []string{strings.TrimPrefix(server.URL, "http://")}, "")
			require.NoError(t, err)
			contracts, err := OpenContracts(rpc, ethcommon.HexToAddress("0x1000").Hex())
			require.NoError(t, err)
			chain := PaymentChain{Contracts: contracts}
			snapshot, err := chain.Snapshot(t.Context())
			require.NoError(t, err)
			expected := big.NewInt(0x100000)
			if l2 {
				expected = big.NewInt(50)
			}
			require.Equal(t, expected, snapshot.Block)
			require.Equal(t, big.NewInt(5), snapshot.Round)
			reorg.Store(true)
			_, err = chain.Snapshot(t.Context())
			require.Error(t, err, "a reorg must not produce mixed observations")
		})
	}
}

func TestRedemptionReceiptRequiresCanonicalFinality(t *testing.T) {
	for _, scenario := range []string{"pending", "unfinalized", "reorg", "confirmed", "reverted"} {
		t.Run(scenario, func(t *testing.T) {
			hash := ethcommon.HexToHash("0xabcd").Hex()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				var result any
				if request.Method == "eth_getTransactionReceipt" {
					if scenario != "pending" {
						status := "0x1"
						if scenario == "reverted" {
							status = "0x0"
						}
						result = map[string]string{"status": status, "blockNumber": "0x10", "blockHash": hash}
					}
				} else {
					var block string
					require.NoError(t, json.Unmarshal(request.Params[0], &block))
					if block == "finalized" {
						number := "0x10"
						if scenario == "unfinalized" {
							number = "0xf"
						}
						result = map[string]string{"number": number}
					} else {
						canonical := hash
						if scenario == "reorg" {
							canonical = ethcommon.HexToHash("0xffff").Hex()
						}
						result = map[string]string{"hash": canonical}
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			}))
			defer server.Close()
			rpc, err := OpenRPC(server.URL, []string{strings.TrimPrefix(server.URL, "http://")}, "")
			require.NoError(t, err)
			confirmed, reverted, err := (PaymentChain{Contracts: &Contracts{RPC: rpc}}).Receipt(t.Context(), ethcommon.HexToHash("0x1234"))
			require.NoError(t, err)
			require.Equal(t, scenario == "confirmed", confirmed)
			require.Equal(t, scenario == "reverted", reverted)
		})
	}
}
