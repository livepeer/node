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
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

// Construct expected ABI bytes independently of the production action builders.
func calldata(signature string, words ...string) string {
	return "0x" + hex.EncodeToString(crypto.Keccak256([]byte(signature))[:4]) + strings.Join(words, "")
}

func uintWord(value uint64) string { return fmt.Sprintf("%064x", value) }
func addressWord(address string) string {
	return strings.Repeat("0", 24) + strings.ToLower(address[2:])
}
func stringResult(value string) string {
	encoded := hex.EncodeToString([]byte(value))
	return uintWord(32) + uintWord(uint64(len(value))) + encoded + strings.Repeat("0", (64-len(encoded)%64)%64)
}

func TestTransactionActionInputs(t *testing.T) {
	// Even valid environment values must not supply optional action inputs or
	// turn a CLI dry run into a submission, wait, config dump, or quiet run.
	for _, name := range []string{"REWARD_CUT", "FEE_SHARE", "LOCK_ID"} {
		t.Setenv("LIVEPEER_CHAIN_"+name, "999")
	}
	t.Setenv("LIVEPEER_CHAIN_DELEGATE", "0x1111111111111111111111111111111111111111")
	t.Setenv("LIVEPEER_CHAIN_SERVICE_URI", "https://environment.example")
	for _, name := range []string{"SUBMIT", "WAIT", "QUIET", "PRINT_CONFIG"} {
		t.Setenv("LIVEPEER_CHAIN_"+name, "true")
	}
	uri := "https://orchestrator.example/live"
	type call struct{ contract, data, value string }
	for _, tc := range []struct {
		command string
		calls   []call
	}{
		{"orchestrator activate --reward-cut 0 --fee-share 1000000", []call{{"BondingManager", calldata("transcoder(uint256,uint256)", uintWord(0), uintWord(1000000)), "0x0"}}},
		{"orchestrator set-config --reward-cut 0 --fee-share 0", []call{{"BondingManager", calldata("transcoder(uint256,uint256)", uintWord(0), uintWord(0)), "0x0"}}},
		{"orchestrator set-config --service-uri " + uri, []call{{"ServiceRegistry", calldata("setServiceURI(string)", stringResult(uri)), "0x0"}}},
		{"orchestrator set-config --reward-cut 3 --fee-share 4 --service-uri " + uri, []call{
			{"BondingManager", calldata("transcoder(uint256,uint256)", uintWord(3), uintWord(4)), "0x0"},
			{"ServiceRegistry", calldata("setServiceURI(string)", stringResult(uri)), "0x0"},
		}},
		{"orchestrator reward", []call{{"BondingManager", calldata("rewardWithHint(address,address)", uintWord(0), uintWord(0)), "0x0"}}},
		{"stake unbond --amount 5", []call{{"BondingManager", calldata("unbondWithHint(uint256,address,address)", uintWord(5), uintWord(0), uintWord(0)), "0x0"}}},
		{"stake rebond --lock-id 0", []call{{"BondingManager", calldata("rebondWithHint(uint256,address,address)", uintWord(0), uintWord(0), uintWord(0)), "0x0"}}},
		{"stake rebond --lock-id 0 --delegate " + testSender, []call{{"BondingManager", calldata("rebondFromUnbondedWithHint(address,uint256,address,address)", addressWord(testSender), uintWord(0), uintWord(0), uintWord(0)), "0x0"}}},
		{"stake withdraw --lock-id 0", []call{{"BondingManager", calldata("withdrawStake(uint256)", uintWord(0)), "0x0"}}},
		{"earnings claim --end-round 0", []call{{"BondingManager", calldata("claimEarnings(uint256)", uintWord(0)), "0x0"}}},
		{"earnings withdraw-fees --amount 5 --recipient " + testSender, []call{{"BondingManager", calldata("withdrawFees(address,uint256)", addressWord(testSender), uintWord(5)), "0x0"}}},
		{"ticketbroker fund --amount 0 --reserve 7", []call{{"TicketBroker", calldata("fundDepositAndReserve(uint256,uint256)", uintWord(0), uintWord(7)), "0x7"}}},
		{"ticketbroker fund --amount 5 --reserve 0", []call{{"TicketBroker", calldata("fundDepositAndReserve(uint256,uint256)", uintWord(5), uintWord(0)), "0x5"}}},
		{"ticketbroker unlock", []call{{"TicketBroker", calldata("unlock()"), "0x0"}}},
		{"ticketbroker cancel-unlock", []call{{"TicketBroker", calldata("cancelUnlock()"), "0x0"}}},
		{"ticketbroker withdraw", []call{{"TicketBroker", calldata("withdraw()"), "0x0"}}},
		{"round initialize", []call{{"RoundsManager", calldata("initializeRound()"), "0x0"}}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			controller := ethcommon.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4")
			addresses := map[string]ethcommon.Address{
				"BondingManager": ethcommon.HexToAddress("0x1000"), "TicketBroker": ethcommon.HexToAddress("0x2000"),
				"ServiceRegistry": ethcommon.HexToAddress("0x3000"), "RoundsManager": ethcommon.HexToAddress("0x4000"), "Minter": ethcommon.HexToAddress("0x5000"),
			}
			var simulated, estimates int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				var result any
				switch req.Method {
				case "eth_chainId":
					result = "0xa4b1"
				case "eth_getBlockByNumber":
					result = &types.Header{Number: big.NewInt(1), Difficulty: new(big.Int), BaseFee: big.NewInt(1)}
				case "eth_maxPriorityFeePerGas":
					result = "0x2"
				case "eth_call", "eth_estimateGas":
					var input struct {
						From, To, Value string
						Data            string `json:"input"`
					}
					require.NoError(t, json.Unmarshal(req.Params[0], &input))
					input.To = ethcommon.HexToAddress(input.To).Hex()
					input.From = ethcommon.HexToAddress(input.From).Hex()
					if req.Method == "eth_call" && (input.To == addresses["BondingManager"].Hex() || input.To == addresses["Minter"].Hex()) {
						if read, ok := stakingReadResult(input.Data); ok {
							result = read
							break
						}
					}
					if input.To == controller.Hex() {
						require.Equal(t, "eth_call", req.Method)
						var contract string
						for name := range addresses {
							if input.Data == calldata("getContract(bytes32)", hex.EncodeToString(crypto.Keccak256([]byte(name)))) {
								contract = name
							}
						}
						require.NotEmpty(t, contract, "unknown Controller lookup: %s", input.Data)
						result = "0x" + addressWord(addresses[contract].Hex())
					} else {
						index := estimates
						if req.Method == "eth_call" {
							index = simulated
							simulated++
						} else {
							estimates++
						}
						require.Less(t, index, len(tc.calls), "unexpected transaction")
						want := tc.calls[index]
						require.Equal(t, addresses[want.contract].Hex(), input.To)
						require.Equal(t, ethcommon.HexToAddress(testSender).Hex(), input.From)
						require.Equal(t, want.data, input.Data)
						require.Equal(t, want.value, input.Value)
						result = "0x5208"
						if req.Method == "eth_call" {
							require.JSONEq(t, `"pending"`, string(req.Params[1]))
							result = "0x"
						}
					}
				default:
					t.Errorf("unexpected RPC method %s", req.Method)
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
			}))
			defer server.Close()
			t.Setenv("LIVEPEER_CHAIN_RPC_URL", server.URL)
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(append(strings.Fields(tc.command), "--sender", testSender, "--output", "json"))
			require.NoError(t, root.Execute())
			require.Equal(t, len(tc.calls), simulated)
			require.Equal(t, simulated, estimates)
			decoder := json.NewDecoder(&output)
			for _, expected := range tc.calls {
				var record struct {
					Submitted  bool
					Simulation struct {
						Data     string
						GasLimit uint64 `json:"gas_limit"`
						FeeCap   string `json:"max_fee_per_gas_wei"`
						TipCap   string `json:"max_priority_fee_per_gas_wei"`
					}
				}
				require.NoError(t, decoder.Decode(&record))
				require.False(t, record.Submitted)
				require.Equal(t, expected.data, record.Simulation.Data)
				require.EqualValues(t, 21000, record.Simulation.GasLimit)
				require.Equal(t, "4", record.Simulation.FeeCap)
				require.Equal(t, "2", record.Simulation.TipCap)
			}
			require.ErrorIs(t, decoder.Decode(new(any)), io.EOF, "emit exactly one plan per simulated transaction")
		})
	}
}

func TestOrchestratorGet(t *testing.T) {
	controller := ethcommon.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4")
	bonding, registry := ethcommon.HexToAddress("0x1000"), ethcommon.HexToAddress("0x2000")
	sender := ethcommon.HexToAddress(testSender)
	uri := "https://orchestrator.example"
	var reads []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string
			Params []json.RawMessage
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		var result string
		switch req.Method {
		case "eth_chainId":
			result = "0xa4b1"
		case "eth_call":
			var input struct {
				To   string
				Data string `json:"input"`
			}
			require.NoError(t, json.Unmarshal(req.Params[0], &input))
			input.To = ethcommon.HexToAddress(input.To).Hex()
			require.JSONEq(t, `"latest"`, string(req.Params[1]))
			switch input.Data {
			case calldata("getContract(bytes32)", hex.EncodeToString(crypto.Keccak256([]byte("BondingManager")))):
				require.Equal(t, controller.Hex(), input.To)
				result = "0x" + addressWord(bonding.Hex())
			case calldata("getContract(bytes32)", hex.EncodeToString(crypto.Keccak256([]byte("ServiceRegistry")))):
				require.Equal(t, controller.Hex(), input.To)
				result = "0x" + addressWord(registry.Hex())
			case calldata("isActiveTranscoder(address)", addressWord(sender.Hex())):
				require.Equal(t, bonding.Hex(), input.To)
				reads = append(reads, "active")
				result = "0x" + uintWord(1)
			case calldata("getServiceURI(address)", addressWord(sender.Hex())):
				require.Equal(t, registry.Hex(), input.To)
				reads = append(reads, "service_uri")
				result = "0x" + stringResult(uri)
			case calldata("getTranscoder(address)", addressWord(sender.Hex())):
				require.Equal(t, bonding.Hex(), input.To)
				reads = append(reads, "transcoder")
				result = "0x" + uintWord(42) + uintWord(12345) + uintWord(67890) + strings.Repeat(uintWord(0), 7)
			default:
				t.Errorf("unexpected contract call: %+v", input)
			}
		default:
			t.Errorf("unexpected RPC method %s", req.Method)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
	}))
	defer server.Close()
	t.Setenv("LIVEPEER_CHAIN_RPC_URL", server.URL)
	var output bytes.Buffer
	root := Root(&output, &output)
	root.SetArgs([]string{"orchestrator", "get", "--sender", testSender, "--output", "json"})
	require.NoError(t, root.Execute())
	require.Equal(t, []string{"active", "service_uri", "transcoder"}, reads)
	require.JSONEq(t, fmt.Sprintf(`{"address":%q,"active":true,"service_uri":%q,"reward_cut":"12345","fee_share":"67890"}`, sender.Hex(), uri), output.String())
}

// An empty pool makes CLI argument tests independent of position arithmetic;
// nonempty old/new positions are exercised by eth's staking behavior tests.
func stakingReadResult(data string) (string, bool) {
	selector := data[:10]
	match := func(sig string) bool { return selector == calldata(sig) }
	switch {
	case match("getTranscoderPoolMaxSize()"):
		return "0x" + uintWord(10), true
	case match("getFirstTranscoderInPool()"):
		return "0x" + uintWord(0), true
	case match("getDelegator(address)"):
		return "0x" + uintWord(20) + uintWord(0) + addressWord(testSender) + strings.Repeat(uintWord(0), 4), true
	case match("transcoderTotalStake(address)"):
		return "0x" + uintWord(20), true
	case match("getDelegatorUnbondingLock(address,uint256)"):
		return "0x" + uintWord(5) + uintWord(10), true
	case match("getTranscoder(address)"):
		return "0x" + strings.Repeat(uintWord(0), 10), true
	case match("getTranscoderEarningsPoolForRound(address,uint256)"):
		return "0x" + uintWord(20) + strings.Repeat(uintWord(0), 4), true
	case match("getTotalBonded()"):
		return "0x" + uintWord(100), true
	case match("currentMintableTokens()"):
		return "0x" + uintWord(10), true
	default:
		return "", false
	}
}
