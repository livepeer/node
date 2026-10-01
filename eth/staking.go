package eth

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth/contracts"
	lpTypes "github.com/livepeer/node/eth/types"
)

type poolEntry struct {
	Address        common.Address
	DelegatedStake *big.Int
}

// Based on go-livepeer's simulateTranscoderPoolUpdate/findTranscoderHints.
// Work on a private slice so old/new delegate calculations cannot mutate each
// other. Updating a member of a full pool must not evict another member.
func poolHints(target common.Address, stake *big.Int, pool []poolEntry, maxSize uint64) lpTypes.TranscoderPoolHints {
	var updated []poolEntry
	for _, entry := range pool {
		if entry.Address != target {
			updated = append(updated, entry)
		}
	}
	if stake.Sign() > 0 {
		updated = append(updated, poolEntry{target, stake})
	}
	sort.SliceStable(updated, func(i, j int) bool { return updated[i].DelegatedStake.Cmp(updated[j].DelegatedStake) > 0 })
	if uint64(len(updated)) > maxSize {
		updated = updated[:maxSize]
	}
	var hints lpTypes.TranscoderPoolHints
	for i, entry := range updated {
		if entry.Address == target {
			if i > 0 {
				hints.PosPrev = updated[i-1].Address
			}
			if i+1 < len(updated) {
				hints.PosNext = updated[i+1].Address
			}
			break
		}
	}
	return hints
}

func (c *Contracts) bonding(ctx context.Context) (*contracts.BondingManagerCaller, common.Address, error) {
	address, err := c.Resolve(ctx, "bondingManager")
	if err != nil {
		return nil, common.Address{}, err
	}
	caller, err := contracts.NewBondingManagerCaller(address, c.callerAt("latest"))
	return caller, address, err
}

func transcoderPool(opts *bind.CallOpts, bonding *contracts.BondingManagerCaller) ([]poolEntry, uint64, error) {
	maxSize, err := bonding.GetTranscoderPoolMaxSize(opts)
	if err != nil {
		return nil, 0, err
	}
	if !maxSize.IsUint64() {
		return nil, 0, errors.New("invalid transcoder pool size")
	}
	address, err := bonding.GetFirstTranscoderInPool(opts)
	if err != nil {
		return nil, 0, err
	}
	var pool []poolEntry
	seen := map[common.Address]bool{}
	for address != (common.Address{}) {
		if seen[address] || uint64(len(pool)) >= maxSize.Uint64() {
			return nil, 0, errors.New("invalid transcoder pool")
		}
		seen[address] = true
		stake, err := bonding.TranscoderTotalStake(opts, address)
		if err != nil {
			return nil, 0, err
		}
		pool = append(pool, poolEntry{address, stake})
		address, err = bonding.GetNextTranscoderInPool(opts, address)
		if err != nil {
			return nil, 0, err
		}
	}
	return pool, maxSize.Uint64(), nil
}

func (c *Contracts) bondData(ctx context.Context, from common.Address, amount *big.Int, to common.Address) ([]byte, common.Address, error) {
	bonding, address, err := c.bonding(ctx)
	if err != nil {
		return nil, address, err
	}
	opts := &bind.CallOpts{Context: ctx}
	pool, maxSize, err := transcoderPool(opts, bonding)
	if err != nil {
		return nil, address, err
	}
	delegator, err := bonding.GetDelegator(opts, from)
	if err != nil {
		return nil, address, err
	}
	delta := new(big.Int).Set(amount)
	var oldHints lpTypes.TranscoderPoolHints
	if delegator.DelegateAddress != to && delegator.DelegateAddress != (common.Address{}) {
		roundsAddr, err := c.Resolve(ctx, "roundsManager")
		if err != nil {
			return nil, address, err
		}
		rounds, err := contracts.NewRoundsManagerCaller(roundsAddr, c.callerAt("latest"))
		if err != nil {
			return nil, address, err
		}
		round, err := rounds.CurrentRound(opts)
		if err != nil {
			return nil, address, err
		}
		pending, err := bonding.PendingStake(opts, from, round)
		if err != nil {
			return nil, address, err
		}
		total, err := bonding.TranscoderTotalStake(opts, delegator.DelegateAddress)
		if err != nil {
			return nil, address, err
		}
		remaining := new(big.Int).Sub(total, pending)
		if remaining.Sign() < 0 {
			return nil, address, errors.New("delegate stake is below pending stake")
		}
		oldHints = poolHints(delegator.DelegateAddress, remaining, pool, maxSize)
		delta.Add(delta, pending)
	}
	total, err := bonding.TranscoderTotalStake(opts, to)
	if err != nil {
		return nil, address, err
	}
	newHints := poolHints(to, new(big.Int).Add(total, delta), pool, maxSize)
	data, err := c.Pack("bondingManager", "bondWithHint", amount, to, oldHints.PosPrev, oldHints.PosNext, newHints.PosPrev, newHints.PosNext)
	return data, address, err
}

func (c *Contracts) stakeData(ctx context.Context, from common.Address, method string, args ...any) ([]byte, common.Address, error) {
	// ABI-compatible values need not have the Go types used below.
	for _, arg := range args {
		amount, isAmount := arg.(*big.Int)
		_, isAddress := arg.(common.Address)
		if !isAddress && (!isAmount || amount == nil) {
			return nil, common.Address{}, errors.New("invalid staking argument")
		}
	}
	if _, err := c.Pack("bondingManager", method, args...); err != nil {
		return nil, common.Address{}, err
	}
	if method == "bond" {
		amount := args[0].(*big.Int)
		if amount.Sign() <= 0 {
			return nil, common.Address{}, errors.New("bond amount must be positive; use ChangeDelegate to move existing stake")
		}
		return c.bondData(ctx, from, amount, args[1].(common.Address))
	}
	bonding, address, err := c.bonding(ctx)
	if err != nil {
		return nil, address, err
	}
	opts := &bind.CallOpts{Context: ctx}
	if method == "reward" || method == "rewardForTranscoder" {
		target := from
		if len(args) != 0 {
			target = args[0].(common.Address)
		}
		hints, err := c.rewardHints(ctx, bonding, target)
		if err != nil {
			return nil, address, err
		}
		data, err := c.Pack("bondingManager", method+"WithHint", append(append([]any{}, args...), hints.PosPrev, hints.PosNext)...)
		return data, address, err
	}
	pool, maxSize, err := transcoderPool(opts, bonding)
	if err != nil {
		return nil, address, err
	}
	var target common.Address
	if method == "rebondFromUnbonded" {
		target = args[0].(common.Address)
	} else {
		delegator, err := bonding.GetDelegator(opts, from)
		if err != nil {
			return nil, address, err
		}
		target = delegator.DelegateAddress
	}
	delta := args[len(args)-1].(*big.Int) // Amount for unbond; lock ID for rebond.
	if method == "unbond" {
		delta = new(big.Int).Neg(delta)
	} else {
		lock, err := bonding.GetDelegatorUnbondingLock(opts, from, delta)
		if err != nil {
			return nil, address, err
		}
		delta = lock.Amount
	}
	total, err := bonding.TranscoderTotalStake(opts, target)
	if err != nil {
		return nil, address, err
	}
	newStake := new(big.Int).Add(total, delta)
	if newStake.Sign() < 0 {
		return nil, address, errors.New("unbond amount exceeds delegate stake")
	}
	hints := poolHints(target, newStake, pool, maxSize)
	args = append(append([]any{}, args...), hints.PosPrev, hints.PosNext)
	data, err := c.Pack("bondingManager", method+"WithHint", args...)
	return data, address, err
}

func (c *Contracts) rewardHints(ctx context.Context, bonding *contracts.BondingManagerCaller, target common.Address) (lpTypes.TranscoderPoolHints, error) {
	opts := &bind.CallOpts{Context: ctx}
	var empty lpTypes.TranscoderPoolHints
	tr, err := bonding.GetTranscoder(opts, target)
	if err != nil {
		return empty, err
	}
	ep, err := bonding.GetTranscoderEarningsPoolForRound(opts, target, tr.LastActiveStakeUpdateRound)
	if err != nil {
		return empty, err
	}
	minterAddr, err := c.Resolve(ctx, "minter")
	if err != nil {
		return empty, err
	}
	minter, err := contracts.NewMinterCaller(minterAddr, c.callerAt("latest"))
	if err != nil {
		return empty, err
	}
	mintable, err := minter.CurrentMintableTokens(opts)
	if err != nil {
		return empty, err
	}
	totalBonded, err := bonding.GetTotalBonded(opts)
	if err != nil {
		return empty, err
	}
	if totalBonded.Sign() == 0 {
		return empty, errors.New("no rewards to be minted")
	}
	reward := new(big.Int).Div(new(big.Int).Mul(mintable, ep.TotalStake), totalBonded)
	stake, err := bonding.TranscoderTotalStake(opts, target)
	if err != nil {
		return empty, err
	}
	pool, maxSize, err := transcoderPool(opts, bonding)
	if err != nil {
		return empty, err
	}
	return poolHints(target, reward.Add(reward, stake), pool, maxSize), nil
}

// PlanContract simulates and estimates a retained operation without signing or
// broadcasting. Staking operations reuse upstream pool-position preparation.
func (c *Contracts) PlanContract(ctx context.Context, from common.Address, name, method string, value *big.Int, args ...any) (TransactionPlan, error) {
	var data []byte
	var address common.Address
	var err error
	if name == "bondingManager" && (method == "bond" || method == "unbond" || method == "rebond" || method == "rebondFromUnbonded" || method == "reward" || method == "rewardForTranscoder") {
		data, address, err = c.stakeData(ctx, from, method, args...)
	} else {
		address, err = c.Resolve(ctx, name)
		if err == nil {
			data, err = c.Pack(name, method, args...)
		}
	}
	var plan TransactionPlan
	if err == nil {
		plan, err = c.PlanTransaction(ctx, from, address, data, value)
	}
	if err != nil {
		return TransactionPlan{}, fmt.Errorf("%s.%s: %w", name, method, err)
	}
	return plan, nil
}

func (c *Contracts) ChangeDelegate(ctx context.Context, from, to common.Address) (TransactionPlan, error) {
	data, address, err := c.bondData(ctx, from, new(big.Int), to)
	if err != nil {
		return TransactionPlan{}, fmt.Errorf("change delegate: %w", err)
	}
	return c.PlanTransaction(ctx, from, address, data, new(big.Int))
}

func (c *Contracts) SetRewardCaller(ctx context.Context, from, caller common.Address) (TransactionPlan, error) {
	return c.PlanContract(ctx, from, "bondingManager", "setRewardCaller", new(big.Int), caller)
}

func (c *Contracts) GetRewardCaller(ctx context.Context, target common.Address) (common.Address, error) {
	bonding, _, err := c.bonding(ctx)
	if err != nil {
		return common.Address{}, err
	}
	return bonding.TranscoderToRewardCaller(&bind.CallOpts{Context: ctx}, target)
}

func (c *Contracts) RewardForTranscoder(ctx context.Context, from, target common.Address) (TransactionPlan, error) {
	return c.PlanContract(ctx, from, "bondingManager", "rewardForTranscoder", new(big.Int), target)
}
