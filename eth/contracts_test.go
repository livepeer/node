package eth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

// Ported in spirit from go-livepeer/eth/client_test.go transaction tests:
// contract resolution, ABI data, simulation, signing and receipt handling.
func TestContractTransactionLifecycle(t *testing.T) {
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	keyPath := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyPath, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	key, err := OpenKeyFile(keyPath)
	require.NoError(t, err)
	controller := ethcommon.HexToAddress("0x1000")
	broker := ethcommon.HexToAddress("0x2000")
	var sent int
	var receipts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		var result any
		switch req.Method {
		case "eth_chainId":
			result = "0x1"
		case "eth_blockNumber":
			result = "0x32"
		case "eth_call":
			var call struct {
				To   string `json:"to"`
				Data string `json:"data"`
			}
			require.NoError(t, json.Unmarshal(req.Params[0], &call))
			if strings.EqualFold(call.To, controller.Hex()) {
				result = "0x" + hex.EncodeToString(ethcommon.LeftPadBytes(broker.Bytes(), 32))
			} else {
				require.Equal(t, broker.Hex(), call.To)
				selector := func(signature string) bool {
					return strings.HasPrefix(call.Data, "0x"+hex.EncodeToString(crypto.Keccak256([]byte(signature))[:4]))
				}
				switch {
				case selector("currentRound()"):
					result = "0x" + fmt.Sprintf("%064x", 5)
				case selector("blockHashForRound(uint256)"):
					result = "0x" + fmt.Sprintf("%064x", 0x1234)
				case selector("getSenderInfo(address)"):
					result = "0x" + fmt.Sprintf("%064x%064x%064x%064x", 10, 0, 1, 0)
				case selector("isActiveTranscoder(address)"):
					result = "0x" + strings.Repeat("0", 63) + "1"
				default:
					result = "0x"
				}
			}
		case "eth_estimateGas":
			result = "0x5208"
		case "eth_gasPrice":
			result = "0x3b9aca00"
		case "eth_getTransactionCount":
			result = "0x7"
		case "eth_sendRawTransaction":
			sent++
			var raw string
			require.NoError(t, json.Unmarshal(req.Params[0], &raw))
			data, err := hex.DecodeString(strings.TrimPrefix(raw, "0x"))
			require.NoError(t, err)
			var tx types.Transaction
			require.NoError(t, tx.UnmarshalBinary(data))
			require.Equal(t, uint64(7), tx.Nonce())
			require.Equal(t, broker, *(tx.To()))
			from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(1)), &tx)
			require.NoError(t, err)
			require.Equal(t, key.Address(), from)
			result = tx.Hash().Hex()
		case "eth_getTransactionReceipt":
			receipts++
			if receipts == 1 {
				result = nil
			} else {
				result = map[string]string{"status": "0x1", "blockNumber": "0x10"}
			}
		default:
			t.Errorf("unexpected RPC method %s", req.Method)
			result = nil
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
	}))
	defer server.Close()
	rpc, err := OpenRPC(server.URL, []string{strings.TrimPrefix(server.URL, "http://")}, "")
	require.NoError(t, err)
	require.NoError(t, rpc.CheckChainID(context.Background(), big.NewInt(1)))
	contracts, err := OpenContracts(rpc, controller.Hex())
	require.NoError(t, err)
	resolved, err := contracts.Resolve(context.Background(), "ticketBroker")
	require.NoError(t, err)
	require.Equal(t, broker, resolved)
	data, err := contracts.Pack("ticketBroker", "unlock")
	require.NoError(t, err)
	plan, err := contracts.PlanTransaction(context.Background(), key.Address(), broker, data, big.NewInt(0))
	require.NoError(t, err)
	require.Equal(t, uint64(21000), plan.GasLimit)
	require.Zero(t, sent, "simulation must not submit")
	hash, err := contracts.Submit(context.Background(), plan, key, big.NewInt(1))
	require.NoError(t, err)
	require.NotEqual(t, ethcommon.Hash{}, hash)
	require.Equal(t, 1, sent)
	result, err := rpc.CallNullable(context.Background(), "eth_getTransactionReceipt", hash.Hex())
	require.NoError(t, err)
	require.Equal(t, "null", string(result))
	block, err := contracts.WaitReceipt(context.Background(), hash)
	require.NoError(t, err)
	require.Equal(t, uint64(16), block)
	confirmed, reverted, err := (PaymentChain{Contracts: contracts}).Receipt(context.Background(), hash)
	require.NoError(t, err)
	require.True(t, confirmed)
	require.False(t, reverted)
	active, err := (PaymentChain{Contracts: contracts}).IsActive(context.Background(), key.Address())
	require.NoError(t, err)
	require.True(t, active)
	snapshot, err := (PaymentChain{Contracts: contracts}).Snapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(50), snapshot.Block.Int64())
	require.Equal(t, int64(5), snapshot.Round.Int64())
	require.Equal(t, ethcommon.HexToHash("0x1234"), snapshot.RoundHash)
	require.NoError(t, (PaymentChain{Contracts: contracts}).ValidateSender(context.Background(), key.Address(), big.NewInt(10)))
	require.Error(t, (PaymentChain{Contracts: contracts}).ValidateSender(context.Background(), key.Address(), big.NewInt(11)))
	redeemedHash, err := (PaymentChain{Contracts: contracts}).Redeem(context.Background(), key, big.NewInt(1), RedeemTicket{Recipient: key.Address(), Sender: ethcommon.HexToAddress("0x3000"), FaceValue: big.NewInt(10), WinProb: big.NewInt(100), SenderNonce: 2, RecipientRandHash: ethcommon.HexToHash("0x4000"), AuxData: make([]byte, 64), Signature: make([]byte, 65), RecipientRand: big.NewInt(7)})
	require.NoError(t, err)
	require.NotEqual(t, ethcommon.Hash{}, redeemedHash)
	require.Equal(t, 2, sent)
}
