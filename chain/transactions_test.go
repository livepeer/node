package chain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

func TestTransactionCommands(t *testing.T) {
	for _, tc := range []struct {
		name          string
		flags         []string
		broadcastErr  bool
		receiptErr    bool
		receiptRevert bool
		simulationErr bool
		wantReceipt   bool
		wantErr       string
		wantSent      int
	}{
		{name: "simulate without a key"},
		{name: "explicit submission", flags: []string{"--submit"}, wantSent: 1},
		{name: "successful receipt", flags: []string{"--submit", "--wait"}, wantReceipt: true, wantSent: 1},
		{name: "receipt failure", flags: []string{"--submit", "--wait"}, receiptErr: true, wantReceipt: true, wantErr: "receipt", wantSent: 1},
		{name: "reverted receipt", flags: []string{"--submit", "--wait"}, receiptRevert: true, wantReceipt: true, wantErr: "reverted", wantSent: 1},
		{name: "failed simulation", flags: []string{"--submit"}, simulationErr: true, wantErr: "simulation failed"},
		{name: "uncertain broadcast", flags: []string{"--submit"}, broadcastErr: true, wantErr: "HTTP 503", wantSent: 1},
		{name: "sender differs from key", flags: []string{"--submit", "--sender", "0x0000000000000000000000000000000000001234"}, wantErr: "sender does not match private key"},
	} {
		for _, mode := range []string{"text", "json"} {
			for _, quiet := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/quiet=%v", tc.name, mode, quiet), func(t *testing.T) {
					dir := t.TempDir()
					keyFile := filepath.Join(dir, "key")
					require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("0", 63)+"1"), 0600))
					key, err := eth.OpenKeyFile(keyFile)
					require.NoError(t, err)
					controller := ethcommon.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4")
					broker := ethcommon.HexToAddress("0x2000")
					deposit, ok := new(big.Int).SetString("100000000000000000001", 10)
					require.True(t, ok)
					reserve := big.NewInt(5)
					value := new(big.Int).Add(deposit, reserve)
					data := append(crypto.Keccak256([]byte("fundDepositAndReserve(uint256,uint256)"))[:4], ethcommon.LeftPadBytes(deposit.Bytes(), 32)...)
					data = append(data, ethcommon.LeftPadBytes(reserve.Bytes(), 32)...)
					var sent, receipts int
					var hash ethcommon.Hash
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var req struct {
							Method string
							Params []json.RawMessage
						}
						require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
						var result any = "0x1"
						switch req.Method {
						case "eth_chainId":
							result = "0xa4b1"
						case "eth_call":
							var call struct{ From, To, Data, Value string }
							require.NoError(t, json.Unmarshal(req.Params[0], &call))
							if call.To == controller.Hex() {
								result = "0x" + hex.EncodeToString(ethcommon.LeftPadBytes(broker.Bytes(), 32))
							} else {
								if tc.simulationErr {
									http.Error(w, "simulation unavailable", http.StatusServiceUnavailable)
									return
								}
								require.Equal(t, broker.Hex(), call.To)
								require.Equal(t, key.Address().Hex(), call.From)
								require.Equal(t, "0x"+hex.EncodeToString(data), call.Data)
								require.Equal(t, "0x"+value.Text(16), call.Value)
								result = "0x"
							}
						case "eth_estimateGas":
							result = "0x5208"
						case "eth_gasPrice":
						case "eth_getTransactionCount":
							require.JSONEq(t, `"`+key.Address().Hex()+`"`, string(req.Params[0]))
							require.JSONEq(t, `"pending"`, string(req.Params[1]))
						case "eth_sendRawTransaction":
							sent++
							var encoded string
							require.NoError(t, json.Unmarshal(req.Params[0], &encoded))
							raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "0x"))
							require.NoError(t, err)
							var tx types.Transaction
							require.NoError(t, tx.UnmarshalBinary(raw))
							require.Equal(t, "42161", tx.ChainId().String())
							require.Equal(t, &broker, tx.To())
							require.Equal(t, value.String(), tx.Value().String())
							require.Equal(t, data, tx.Data())
							require.EqualValues(t, 1, tx.Nonce())
							sender, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
							require.NoError(t, err)
							require.Equal(t, key.Address(), sender)
							hash = tx.Hash()
							if tc.broadcastErr {
								http.Error(w, "broadcast response unavailable", http.StatusServiceUnavailable)
								return
							}
							result = hash.Hex()
						case "eth_getTransactionReceipt":
							receipts++
							if tc.receiptErr {
								http.Error(w, "receipt unavailable", http.StatusServiceUnavailable)
								return
							}
							status := "0x1"
							if tc.receiptRevert {
								status = "0x0"
							}
							result = map[string]string{"status": status, "blockNumber": "0x10"}
						default:
							t.Errorf("unexpected RPC method %s", req.Method)
						}
						require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
					}))
					defer server.Close()
					rpcFile := filepath.Join(dir, "rpc")
					require.NoError(t, os.WriteFile(rpcFile, []byte(server.URL), 0600))
					configFile := filepath.Join(dir, "chain.toml")
					config := fmt.Sprintf("RPCURLFile = %q\nSender = %q\n", rpcFile, key.Address().Hex())
					if slices.Contains(tc.flags, "--submit") {
						config += fmt.Sprintf("KeyFile = %q\n", keyFile)
					}
					require.NoError(t, os.WriteFile(configFile, []byte(config), 0600))
					var output bytes.Buffer
					root := Root(&output, &output)
					args := append([]string{"ticketbroker", "fund", "--config", configFile, "--amount", deposit.String(), "--reserve", reserve.String(), "--output", mode}, tc.flags...)
					if quiet {
						args = append(args, "--quiet")
					}
					root.SetArgs(args)
					err = root.Execute()
					if tc.wantErr == "" {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, tc.wantErr)
					}
					require.Equal(t, tc.wantSent, sent)
					require.Equal(t, tc.wantReceipt, receipts > 0)
					if sent > 0 && tc.wantErr != "" {
						require.ErrorContains(t, err, hash.Hex(), "the operator must be able to reconcile this transaction")
					}
					if quiet {
						require.Empty(t, output.String())
						return
					}
					if tc.wantErr != "" && sent == 0 {
						require.Empty(t, output.String())
						return
					}
					if mode == "text" {
						require.Contains(t, output.String(), "\n  \"command\":")
					} else {
						require.NotContains(t, output.String(), "\n  \"command\":")
					}
					var record struct {
						Submitted       bool   `json:"submitted"`
						TransactionHash string `json:"transaction_hash"`
						SubmissionError string `json:"submission_error"`
						Simulation      eth.TransactionPlan
					}
					decoder := json.NewDecoder(&output)
					require.NoError(t, decoder.Decode(&record))
					require.Equal(t, value.String(), record.Simulation.ValueWei)
					require.Equal(t, sent == 1 && !tc.broadcastErr, record.Submitted)
					if sent == 0 {
						require.Empty(t, record.TransactionHash)
					} else {
						require.Equal(t, hash.Hex(), record.TransactionHash)
					}
					if tc.broadcastErr {
						require.Contains(t, record.SubmissionError, tc.wantErr)
					}
					if tc.wantReceipt && tc.wantErr == "" {
						var receipt map[string]any
						require.NoError(t, decoder.Decode(&receipt))
						require.Equal(t, hash.Hex(), receipt["transaction_hash"])
						require.EqualValues(t, 16, receipt["confirmed_block"])
					}
					require.ErrorIs(t, decoder.Decode(new(any)), io.EOF)
				})
			}
		}
	}
}

func TestBondApprovalSequencing(t *testing.T) {
	for _, tc := range []struct {
		name         string
		flags        []string
		allowance    uint64
		balance      *uint64
		revert       bool
		wantErr      string
		wantMethods  []string
		wantSent     int
		wantReceipts int
	}{
		{name: "dry run plans approval", wantMethods: []string{"approve"}},
		{name: "submit stops after approval", flags: []string{"--submit"}, wantMethods: []string{"approve"}, wantSent: 1},
		{name: "wait confirms approval before bond", flags: []string{"--submit", "--wait"}, wantMethods: []string{"approve", "bond"}, wantSent: 2, wantReceipts: 2},
		{name: "existing allowance", allowance: 5, wantMethods: []string{"bond"}},
		{name: "insufficient balance", balance: pointer(uint64(4)), wantErr: "insufficient Livepeer token balance"},
		{name: "approval reverts", flags: []string{"--submit", "--wait"}, revert: true, wantMethods: []string{"approve"}, wantSent: 1, wantReceipts: 1, wantErr: "reverted"},
	} {
		for _, quiet := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/quiet=%v", tc.name, quiet), func(t *testing.T) {
				keyFile := filepath.Join(t.TempDir(), "key")
				require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("0", 63)+"1"), 0600))
				key, err := eth.OpenKeyFile(keyFile)
				require.NoError(t, err)
				controller := ethcommon.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4")
				token := ethcommon.HexToAddress("0x2000")
				bonding := ethcommon.HexToAddress("0x3000")
				var planned []string
				var sent, receipts int
				var lastHash string
				approved := tc.allowance >= 5
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						Method string
						Params []json.RawMessage
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					var result any = "0x1"
					switch req.Method {
					case "eth_chainId", "eth_gasPrice":
					case "eth_getTransactionCount":
						result = fmt.Sprintf("0x%x", sent)
					case "eth_estimateGas":
						result = "0x5208"
					case "eth_call":
						var call struct{ To, Data string }
						require.NoError(t, json.Unmarshal(req.Params[0], &call))
						switch call.To {
						case controller.Hex():
							address := bonding
							if strings.HasSuffix(call.Data, hex.EncodeToString(crypto.Keccak256([]byte("LivepeerToken")))) {
								address = token
							}
							result = "0x" + hex.EncodeToString(ethcommon.LeftPadBytes(address.Bytes(), 32))
						case token.Hex():
							selector := call.Data[:10]
							switch selector {
							case "0x" + hex.EncodeToString(crypto.Keccak256([]byte("balanceOf(address)"))[:4]):
								require.Equal(t, calldata("balanceOf(address)", addressWord(key.Address().Hex())), call.Data)
								balance := uint64(5)
								if tc.balance != nil {
									balance = *tc.balance
								}
								result = "0x" + uintWord(balance)
							case "0x" + hex.EncodeToString(crypto.Keccak256([]byte("allowance(address,address)"))[:4]):
								require.Equal(t, calldata("allowance(address,address)", addressWord(key.Address().Hex()), addressWord(bonding.Hex())), call.Data)
								result = "0x" + uintWord(tc.allowance)
							default:
								require.Equal(t, calldata("approve(address,uint256)", addressWord(bonding.Hex()), uintWord(5)), call.Data)
								planned = append(planned, "approve")
								result = "0x" + uintWord(1)
							}
						case bonding.Hex():
							require.True(t, approved, "bond must not run before approval confirms")
							require.Equal(t, calldata("bond(uint256,address)", uintWord(5), addressWord(testSender)), call.Data, "positional target and amount must reach the bond ABI")
							planned = append(planned, "bond")
							result = "0x"
						default:
							t.Errorf("unexpected contract %s", call.To)
						}
					case "eth_sendRawTransaction":
						var encoded string
						require.NoError(t, json.Unmarshal(req.Params[0], &encoded))
						raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "0x"))
						require.NoError(t, err)
						var tx types.Transaction
						require.NoError(t, tx.UnmarshalBinary(raw))
						require.Equal(t, uint64(sent), tx.Nonce())
						expectedTo, expectedData := bonding, calldata("bond(uint256,address)", uintWord(5), addressWord(testSender))
						if planned[len(planned)-1] == "approve" {
							expectedTo, expectedData = token, calldata("approve(address,uint256)", addressWord(bonding.Hex()), uintWord(5))
						}
						require.Equal(t, &expectedTo, tx.To())
						require.Equal(t, expectedData, "0x"+hex.EncodeToString(tx.Data()))
						require.Zero(t, tx.Value().Sign())
						sent++
						lastHash = tx.Hash().Hex()
						result = lastHash
					case "eth_getTransactionReceipt":
						require.JSONEq(t, strconv.Quote(lastHash), string(req.Params[0]))
						receipts++
						approved = !tc.revert
						status := "0x1"
						if tc.revert {
							status = "0x0"
						}
						result = map[string]string{"status": status, "blockNumber": "0x10"}
					default:
						t.Errorf("unexpected RPC %s", req.Method)
					}
					require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
				}))
				defer server.Close()
				t.Setenv("LIVEPEER_CHAIN_RPC_URL", server.URL)
				args := []string{"stake", "bond", testSender, "--amount", "5", "--sender", key.Address().Hex(), "--output", "json"}
				args = append(args, tc.flags...)
				if slices.Contains(tc.flags, "--submit") {
					args = append(args, "--private-key-file", keyFile)
				}
				if quiet {
					args = append(args, "--quiet")
				}
				var output bytes.Buffer
				root := Root(&output, &output)
				root.SetArgs(args)
				err = root.Execute()
				if tc.wantErr == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tc.wantErr)
					if sent > 0 {
						require.ErrorContains(t, err, lastHash)
					}
				}
				require.Equal(t, tc.wantMethods, planned)
				require.Equal(t, tc.wantSent, sent)
				require.Equal(t, tc.wantReceipts, receipts)
				if quiet || tc.balance != nil {
					require.Empty(t, output.String())
				} else {
					require.NotEmpty(t, output.String())
					if tc.allowance < 5 {
						require.Contains(t, output.String(), "bond after approval confirms")
					}
				}
			})
		}
	}
}
