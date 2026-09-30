package eth

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestPriceFeedConversionAndRejection(t *testing.T) {
	for _, scenario := range []string{"valid", "stale", "future", "negative", "incomplete", "wrong-pair"} {
		t.Run(scenario, func(t *testing.T) {
			contract, err := abi.JSON(strings.NewReader(contractABIs["priceFeed"]))
			require.NoError(t, err)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				var result any
				if req.Method == "eth_getBlockByNumber" {
					result = map[string]string{"hash": ethcommon.HexToHash("0x1234").Hex()}
				} else {
					var call map[string]string
					require.NoError(t, json.Unmarshal(req.Params[0], &call))
					data, err := hex.DecodeString(strings.TrimPrefix(call["data"], "0x"))
					require.NoError(t, err)
					method, err := contract.MethodById(data[:4])
					require.NoError(t, err)
					var args []any
					switch method.Name {
					case "description":
						pair := "ETH / USD"
						if scenario == "wrong-pair" {
							pair = "BTC / USD"
						}
						args = []any{pair}
					case "decimals":
						args = []any{uint8(8)}
					case "latestRoundData":
						answer, updated, answered := big.NewInt(2000_00000000), time.Now().Add(-time.Minute).Unix(), big.NewInt(10)
						switch scenario {
						case "stale":
							updated = time.Now().Add(-3 * time.Hour).Unix()
						case "future":
							updated = time.Now().Add(time.Hour).Unix()
						case "negative":
							answer.Neg(answer)
						case "incomplete":
							answered = big.NewInt(9)
						}
						args = []any{big.NewInt(10), answer, big.NewInt(updated), big.NewInt(updated), answered}
					}
					packed, err := method.Outputs.Pack(args...)
					require.NoError(t, err)
					result = "0x" + hex.EncodeToString(packed)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			}))
			defer server.Close()
			rpc, err := OpenRPC(server.URL, []string{strings.TrimPrefix(server.URL, "http://")}, "")
			require.NoError(t, err)
			contracts, err := OpenContracts(rpc, ethcommon.HexToAddress("0x1000").Hex())
			require.NoError(t, err)
			rate, until, err := contracts.WeiPerUSD(t.Context(), ethcommon.HexToAddress("0x2000"), 2*time.Hour)
			if scenario != "valid" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, big.NewRat(500_000_000_000_000, 1), rate)
			require.True(t, until.After(time.Now()))
		})
	}
}
