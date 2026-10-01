package eth

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	lpTypes "github.com/livepeer/node/eth/types"
	"github.com/stretchr/testify/require"
)

func word(n int64) string                 { return fmt.Sprintf("%064x", n) }
func addressWord(a common.Address) string { return fmt.Sprintf("%064x", a.Bytes()) }
func calldata(signature string, words ...string) string {
	return hexutil.Encode(crypto.Keccak256([]byte(signature))[:4]) + strings.Join(words, "")
}

func TestHintedStakingAndRetainedAPIs(t *testing.T) {
	caller, old, other, target := common.HexToAddress("0x0100"), common.HexToAddress("0x1001"), common.HexToAddress("0x1002"), common.HexToAddress("0x1003")
	controller, bonding, rounds, minter, governor, poll := common.HexToAddress("0x2000"), common.HexToAddress("0x2001"), common.HexToAddress("0x2002"), common.HexToAddress("0x2003"), common.HexToAddress("0x2004"), common.HexToAddress("0x3000")
	zero := word(0)
	for _, tc := range []struct {
		name         string
		run          func(*Contracts) (TransactionPlan, error)
		from, to     common.Address
		data         string
		rewardTarget common.Address
	}{
		{"read authorized reward caller", func(c *Contracts) (TransactionPlan, error) {
			authorized, err := c.GetRewardCaller(t.Context(), target)
			require.Equal(t, caller, authorized)
			return TransactionPlan{}, err
		}, common.Address{}, common.Address{}, "", common.Address{}},
		{"bond while changing delegate", func(c *Contracts) (TransactionPlan, error) {
			return c.PlanContract(t.Context(), caller, "bondingManager", "bond", new(big.Int), big.NewInt(5), target)
		}, caller, bonding,
			calldata("bondWithHint(uint256,address,address,address,address,address)", word(5), addressWord(target), addressWord(other), addressWord(target), addressWord(other), zero), common.Address{}},
		{"change delegate without new tokens", func(c *Contracts) (TransactionPlan, error) { return c.ChangeDelegate(t.Context(), caller, target) }, caller, bonding,
			calldata("bondWithHint(uint256,address,address,address,address,address)", zero, addressWord(target), addressWord(other), addressWord(target), addressWord(other), zero), common.Address{}},
		{"unbond changes pool position", func(c *Contracts) (TransactionPlan, error) {
			return c.PlanContract(t.Context(), caller, "bondingManager", "unbond", new(big.Int), big.NewInt(30))
		}, caller, bonding,
			calldata("unbondWithHint(uint256,address,address)", word(30), addressWord(other), addressWord(target)), common.Address{}},
		{"rebond lock amount", func(c *Contracts) (TransactionPlan, error) {
			return c.PlanContract(t.Context(), caller, "bondingManager", "rebond", new(big.Int), big.NewInt(7))
		}, caller, bonding,
			calldata("rebondWithHint(uint256,address,address)", word(7), zero, addressWord(other)), common.Address{}},
		{"rebond from unbonded", func(c *Contracts) (TransactionPlan, error) {
			return c.PlanContract(t.Context(), caller, "bondingManager", "rebondFromUnbonded", new(big.Int), target, big.NewInt(7))
		}, caller, bonding,
			calldata("rebondFromUnbondedWithHint(address,uint256,address,address)", addressWord(target), word(7), addressWord(other), zero), common.Address{}},
		{"self reward", func(c *Contracts) (TransactionPlan, error) {
			return c.PlanContract(t.Context(), old, "bondingManager", "reward", new(big.Int))
		}, old, bonding,
			calldata("rewardWithHint(address,address)", zero, addressWord(other)), old},
		{"delegated reward uses target stake", func(c *Contracts) (TransactionPlan, error) { return c.RewardForTranscoder(t.Context(), caller, target) }, caller, bonding,
			calldata("rewardForTranscoderWithHint(address,address,address)", addressWord(target), addressWord(other), zero), target},
		{"authorize reward caller", func(c *Contracts) (TransactionPlan, error) { return c.SetRewardCaller(t.Context(), old, caller) }, old, bonding,
			calldata("setRewardCaller(address)", addressWord(caller)), common.Address{}},
		{"revoke reward caller", func(c *Contracts) (TransactionPlan, error) {
			return c.SetRewardCaller(t.Context(), old, common.Address{})
		}, old, bonding,
			calldata("setRewardCaller(address)", zero), common.Address{}},
		{"poll vote uses explicit address", func(c *Contracts) (TransactionPlan, error) { return c.Vote(t.Context(), caller, poll, lpTypes.No) }, caller, poll,
			calldata("vote(uint256)", word(1)), common.Address{}},
		{"proposal vote resolves governor", func(c *Contracts) (TransactionPlan, error) {
			return c.ProposalVote(t.Context(), caller, big.NewInt(5), lpTypes.Abstain)
		}, caller, governor,
			calldata("castVote(uint256,uint8)", word(5), word(2)), common.Address{}},
		{"proposal vote reason", func(c *Contracts) (TransactionPlan, error) {
			return c.ProposalVoteWithReason(t.Context(), caller, big.NewInt(5), lpTypes.For, "abc")
		}, caller, governor,
			calldata("castVoteWithReason(uint256,uint8,string)", word(5), word(1), word(96), word(3), "616263"+strings.Repeat("0", 58)), common.Address{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var simulations int
			rpc := testRPC(t, func(method string, params []json.RawMessage) any {
				if tc.data == "" {
					require.Equal(t, "eth_call", method, "reading authorization must only read")
				}
				var result any
				switch method {
				case "eth_getBlockByNumber":
					result = &types.Header{Number: big.NewInt(1), Difficulty: new(big.Int), BaseFee: big.NewInt(1)}
				case "eth_maxPriorityFeePerGas":
					result = "0x2"
				case "eth_estimateGas":
					result = "0x5208"
				case "eth_call":
					var call struct {
						From, To common.Address
						Input    string
						Value    string
					}
					require.NoError(t, json.Unmarshal(params[0], &call))
					var block string
					require.NoError(t, json.Unmarshal(params[1], &block))
					if block == "pending" {
						simulations++
						require.Equal(t, tc.from, call.From)
						require.Equal(t, tc.to, call.To)
						require.Equal(t, tc.data, call.Input)
						require.Equal(t, "0x0", call.Value)
						result = "0x"
						break
					}
					input, err := hexutil.Decode(call.Input)
					require.NoError(t, err)
					selector := call.Input[:10]
					arg := func(i int) common.Address { return common.BytesToAddress(input[4+32*i : 4+32*(i+1)]) }
					sig := func(s string) bool { return selector == calldata(s) }
					switch {
					case call.To == controller:
						addresses := map[string]common.Address{"BondingManager": bonding, "RoundsManager": rounds, "Minter": minter, "LivepeerGovernor": governor}
						for name, address := range addresses {
							if call.Input == calldata("getContract(bytes32)", hexutil.Encode(crypto.Keccak256([]byte(name)))[2:]) {
								result = "0x" + addressWord(address)
							}
						}
						require.NotNil(t, result, "unexpected Controller lookup")
					case sig("transcoderToRewardCaller(address)"):
						require.Equal(t, bonding, call.To)
						require.Equal(t, calldata("transcoderToRewardCaller(address)", addressWord(target)), call.Input)
						result = "0x" + addressWord(caller)
					case sig("getTranscoderPoolMaxSize()"):
						result = "0x" + word(3)
					case sig("getFirstTranscoderInPool()"):
						result = "0x" + addressWord(old)
					case sig("getNextTranscoderInPool(address)"):
						next := map[common.Address]common.Address{old: other, other: target, target: {}}[arg(0)]
						result = "0x" + addressWord(next)
					case sig("transcoderTotalStake(address)"):
						stake, ok := map[common.Address]int64{old: 100, other: 80, target: 40}[arg(0)]
						require.True(t, ok, "caller stake must not be used for target hints")
						result = "0x" + word(stake)
					case sig("getDelegator(address)"):
						require.Equal(t, caller, arg(0))
						result = "0x" + word(20) + zero + addressWord(old) + strings.Repeat(zero, 4)
					case sig("currentRound()"):
						result = "0x" + word(9)
					case sig("pendingStake(address,uint256)"):
						require.Equal(t, calldata("pendingStake(address,uint256)", addressWord(caller), word(9)), call.Input)
						result = "0x" + word(20)
					case sig("getDelegatorUnbondingLock(address,uint256)"):
						require.Equal(t, calldata("getDelegatorUnbondingLock(address,uint256)", addressWord(caller), word(7)), call.Input)
						result = "0x" + word(10) + word(12)
					case sig("getTranscoder(address)"):
						require.Equal(t, tc.rewardTarget, arg(0))
						result = "0x" + strings.Repeat(zero, 3) + word(7) + strings.Repeat(zero, 6)
					case sig("getTranscoderEarningsPoolForRound(address,uint256)"):
						require.Equal(t, calldata("getTranscoderEarningsPoolForRound(address,uint256)", addressWord(tc.rewardTarget), word(7)), call.Input)
						result = "0x" + word(20) + strings.Repeat(zero, 4)
					case sig("currentMintableTokens()"):
						require.Equal(t, minter, call.To)
						result = "0x" + word(200)
					case sig("getTotalBonded()"):
						result = "0x" + word(100)
					default:
						t.Fatalf("unexpected contract call %s", call.Input)
					}
				default:
					t.Fatalf("unexpected RPC method %s", method)
				}
				return result
			})
			c, err := NewContracts(rpc, controller)
			require.NoError(t, err)
			plan, err := tc.run(c)
			require.NoError(t, err)
			require.Equal(t, tc.data, plan.Data)
			if tc.data == "" {
				require.Zero(t, simulations, "reads must not simulate")
			} else {
				require.Equal(t, 1, simulations, "transaction APIs must simulate exactly once")
			}
		})
	}
}

func TestStakingRejectsUnexpectedArgumentTypes(t *testing.T) {
	c, err := NewContracts(nil, common.Address{})
	require.NoError(t, err)
	for _, args := range [][]any{{big.NewInt(1), [20]byte{}}, {(*big.Int)(nil), common.Address{}}} {
		_, err := c.PlanContract(t.Context(), common.Address{}, "bondingManager", "bond", new(big.Int), args...)
		require.ErrorContains(t, err, "invalid staking argument")
	}
}
