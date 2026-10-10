package chain

import (
	"context"
	"errors"
	"math/big"
	"net/url"

	"github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/eth"
)

func contractAction(contract, method string, args ...any) []action {
	return []action{{Contract: contract, Method: method, Args: args, Value: new(big.Int)}}
}
func resolvedActions(resolve func(context.Context, *eth.Contracts, common.Address) ([]action, error)) []action {
	return []action{{resolve: resolve}}
}
func validateURI(u *url.URL) error {
	if u != nil {
		if err := destination.ValidateURL(u); err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("service-uri must be an absolute HTTP or HTTPS URL")
		}
	}
	return nil
}
func (p PercentageOptions) commissions() (*big.Int, *big.Int, error) {
	var reward, share *big.Int
	var err error
	if p.RewardCut != nil {
		reward, err = parsePercent(*p.RewardCut)
		if err != nil {
			return nil, nil, err
		}
	}
	if p.FeeCut != nil {
		cut, err := parsePercent(*p.FeeCut)
		if err != nil {
			return nil, nil, err
		}
		share = new(big.Int).Sub(big.NewInt(1000000), cut)
	}
	return reward, share, nil
}
func configActions(ctx context.Context, snapshot *eth.Inspection, from common.Address, reward, share *big.Int, u *url.URL) ([]action, error) {
	var result []action
	if reward != nil || share != nil {
		tr, err := snapshot.Call(ctx, "bondingManager", "getTranscoder", from)
		if err != nil {
			return nil, err
		}
		if reward == nil {
			reward = tr[1].(*big.Int)
		}
		if share == nil {
			share = tr[2].(*big.Int)
		}
		if reward.Cmp(tr[1].(*big.Int)) != 0 || share.Cmp(tr[2].(*big.Int)) != 0 {
			locked, err := snapshot.Call(ctx, "roundsManager", "currentRoundLocked")
			if err != nil {
				return nil, err
			}
			if locked[0].(bool) {
				return nil, errors.New("cannot change commissions while the current round is locked")
			}
			result = append(result, contractAction("bondingManager", "transcoder", reward, share)...)
		}
	}
	if u != nil {
		uri, err := snapshot.Call(ctx, "serviceRegistry", "getServiceURI", from)
		if err != nil {
			return nil, err
		}
		if u.String() != uri[0].(string) {
			result = append(result, contractAction("serviceRegistry", "setServiceURI", u.String())...)
		}
	}
	return result, nil
}
func (p SetConfigParams) actions() ([]action, error) {
	if p.RewardCut == nil && p.FeeCut == nil && p.ServiceURI.Value == nil {
		return nil, errors.New("reward-cut, fee-cut or service-uri is required")
	}
	reward, share, err := p.commissions()
	if err != nil {
		return nil, err
	}
	if err := validateURI(p.ServiceURI.Value); err != nil {
		return nil, err
	}
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		snapshot, err := c.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		return configActions(ctx, snapshot, from, reward, share, p.ServiceURI.Value)
	}), nil
}
func bondActions(ctx context.Context, c *eth.Contracts, from, to common.Address, raw string, base, redelegate bool) ([]action, error) {
	if to == (common.Address{}) {
		return nil, errors.New("orchestrator address must be nonempty")
	}
	if redelegate {
		s, err := c.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		stake, err := s.Stake(ctx, from)
		if err != nil {
			return nil, err
		}
		pending, _ := new(big.Int).SetString(stake.PendingStake, 10)
		if stake.Status == "Unbonded" || pending.Sign() == 0 {
			return nil, errors.New("redelegation requires existing stake")
		}
		if stake.Delegate == to {
			return nil, nil
		}
		return contractAction("bondingManager", "changeDelegate", to), nil
	}
	amount, err := parseAmount(raw, base, true, true)
	if err != nil {
		return nil, err
	}
	token, err := c.Resolve(ctx, "livepeerToken")
	if err != nil {
		return nil, err
	}
	balance, err := c.Call(ctx, "livepeerToken", token, "balanceOf", from)
	if err != nil {
		return nil, err
	}
	wallet := balance[0].(*big.Int)
	if amount == nil {
		amount = new(big.Int).Set(wallet)
	}
	if amount.Sign() == 0 {
		return nil, errors.New("wallet LPT balance is zero")
	}
	if wallet.Cmp(amount) < 0 {
		return nil, errors.New("insufficient Livepeer token balance")
	}
	bonding, err := c.Resolve(ctx, "bondingManager")
	if err != nil {
		return nil, err
	}
	allowance, err := c.Call(ctx, "livepeerToken", token, "allowance", from, bonding)
	if err != nil {
		return nil, err
	}
	var result []action
	if allowance[0].(*big.Int).Cmp(amount) < 0 {
		result = contractAction("livepeerToken", "approve", bonding, amount)
	}
	return append(result, contractAction("bondingManager", "bond", amount, to)...), nil
}
func validateBondMode(amount string, redelegate bool) error {
	if (amount != "") == redelegate {
		return errors.New("choose exactly one of amount or redelegate")
	}
	return nil
}
func (p BondParams) actions() ([]action, error) {
	if err := validateBondMode(p.Amount, p.Redelegate); err != nil {
		return nil, err
	}
	if !p.Redelegate {
		if _, err := parseAmount(p.Amount, p.BaseUnits, true, true); err != nil {
			return nil, err
		}
	}
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		return bondActions(ctx, c, from, p.Orchestrator, p.Amount, p.BaseUnits, p.Redelegate)
	}), nil
}
func (p RegisterParams) actions() ([]action, error) {
	if p.RewardCut == nil || p.FeeCut == nil {
		return nil, errors.New("reward-cut and fee-cut are required")
	}
	reward, share, err := p.commissions()
	if err != nil {
		return nil, err
	}
	if err := validateURI(p.ServiceURI.Value); err != nil {
		return nil, err
	}
	modes := 0
	if p.Amount != "" {
		modes++
	}
	if p.Redelegate {
		modes++
	}
	if p.LockID != nil {
		modes++
	}
	if modes > 1 {
		return nil, errors.New("amount, redelegate and lock-id are mutually exclusive")
	}
	if p.Amount != "" {
		if _, err := parseAmount(p.Amount, p.BaseUnits, true, true); err != nil {
			return nil, err
		}
	}
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		snapshot, err := c.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		stake, err := snapshot.Stake(ctx, from)
		if err != nil {
			return nil, err
		}
		self := stake.Status != "Unbonded" && stake.Delegate == from
		if !self && modes == 0 {
			return nil, errors.New("registration requires amount, redelegate or lock-id when not self-bonded")
		}
		var result []action
		switch {
		case p.Amount != "" || p.Redelegate:
			result, err = bondActions(ctx, c, from, from, p.Amount, p.BaseUnits, p.Redelegate)
		case p.LockID != nil:
			lock := p.LockID.ToBig()
			values, lockErr := snapshot.Call(ctx, "bondingManager", "getDelegatorUnbondingLock", from, lock)
			if lockErr != nil {
				return nil, lockErr
			}
			if values[1].(*big.Int).Sign() == 0 {
				return nil, errors.New("unbonding lock does not exist")
			}
			if stake.Status == "Unbonded" {
				result = contractAction("bondingManager", "rebondFromUnbonded", from, lock)
			} else {
				if !self {
					result = append(result, contractAction("bondingManager", "changeDelegate", from)...)
				}
				result = append(result, contractAction("bondingManager", "rebond", lock)...)
			}
		}
		if err != nil {
			return nil, err
		}
		config, err := configActions(ctx, snapshot, from, reward, share, p.ServiceURI.Value)
		if err != nil {
			return nil, err
		}
		return append(result, config...), nil
	}), nil
}
func (p CancelUnbondParams) actions() ([]action, error) {
	if p.Delegate != nil && *p.Delegate == (common.Address{}) {
		return nil, errors.New("delegate must be nonempty")
	}
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		s, err := c.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		stake, err := s.Stake(ctx, from)
		if err != nil {
			return nil, err
		}
		if stake.Status == "Unbonded" {
			if p.Delegate == nil {
				return nil, errors.New("an address is required to cancel unbonding when unbonded")
			}
			return contractAction("bondingManager", "rebondFromUnbonded", *p.Delegate, p.LockID.ToBig()), nil
		}
		if p.Delegate != nil && *p.Delegate != stake.Delegate {
			return nil, errors.New("supplied address does not match the existing delegate")
		}
		return contractAction("bondingManager", "rebond", p.LockID.ToBig()), nil
	}), nil
}
func amountAction(contract, method, raw string, base bool, kind string, recipient *common.Address) ([]action, error) {
	amount, err := parseAmount(raw, base, true, true)
	if err != nil {
		return nil, err
	}
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		resolved := amount
		if resolved == nil {
			s, err := c.Inspect(ctx)
			if err != nil {
				return nil, err
			}
			var text string
			switch kind {
			case "wallet":
				values, err := s.Call(ctx, "livepeerToken", "balanceOf", from)
				if err != nil {
					return nil, err
				}
				text = values[0].(*big.Int).String()
			default:
				stake, err := s.Stake(ctx, from)
				if err != nil {
					return nil, err
				}
				if kind == "stake" {
					text = stake.PendingStake
				} else {
					text = stake.PendingFees
				}
			}
			resolved, _ = new(big.Int).SetString(text, 10)
		}
		if resolved.Sign() == 0 {
			return nil, errors.New("resolved amount is zero")
		}
		switch method {
		case "transfer", "withdrawFees":
			to := from
			if recipient != nil {
				to = *recipient
			}
			if to == (common.Address{}) {
				return nil, errors.New("recipient must be nonempty")
			}
			return contractAction(contract, method, to, resolved), nil
		default:
			return contractAction(contract, method, resolved), nil
		}
	}), nil
}
func (p UnbondParams) actions() ([]action, error) {
	return amountAction("bondingManager", "unbond", p.Amount, p.BaseUnits, "stake", nil)
}
func (p WithdrawFeesParams) actions() ([]action, error) {
	return amountAction("bondingManager", "withdrawFees", p.Amount, p.BaseUnits, "fees", p.Recipient)
}
func (p TransferParams) actions() ([]action, error) {
	return amountAction("livepeerToken", "transfer", p.Amount, p.BaseUnits, "wallet", &p.Recipient)
}
func (p WithdrawStakeParams) actions() ([]action, error) {
	return contractAction("bondingManager", "withdrawStake", p.LockID.ToBig()), nil
}
func (p ClaimParams) actions() ([]action, error) {
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		end := p.EndRound
		if end != nil {
			return contractAction("bondingManager", "claimEarnings", end.ToBig()), nil
		}
		s, err := c.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		round, err := s.Round(ctx)
		if err != nil {
			return nil, err
		}
		if !round.Initialized {
			return nil, errors.New("current round is not initialized; initialize the round before claiming")
		}
		value, _ := new(big.Int).SetString(round.CurrentRound, 10)
		return contractAction("bondingManager", "claimEarnings", value), nil
	}), nil
}
func (p FundParams) actions() ([]action, error) {
	deposit, err := parseAmount(p.Amount, p.BaseUnits, false, false)
	if err != nil {
		return nil, err
	}
	reserve, err := parseAmount(p.Reserve, p.BaseUnits, false, false)
	if err != nil {
		return nil, err
	}
	value := new(big.Int).Add(deposit, reserve)
	if value.BitLen() > 256 {
		return nil, errors.New("deposit and reserve total exceeds uint256")
	}
	if value.Sign() == 0 {
		return nil, errors.New("deposit and reserve cannot both be zero")
	}
	return []action{{Contract: "ticketBroker", Method: "fundDepositAndReserve", Args: []any{deposit, reserve}, Value: value}}, nil
}
func (p RewardParams) actions() ([]action, error) {
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		target := from
		if p.Orchestrator != nil {
			target = *p.Orchestrator
		}
		s, err := c.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		active, err := s.Call(ctx, "bondingManager", "isActiveTranscoder", target)
		if err != nil {
			return nil, err
		}
		if !active[0].(bool) {
			return nil, errors.New("reward target is not active")
		}
		if target == from {
			return contractAction("bondingManager", "reward"), nil
		}
		return contractAction("bondingManager", "rewardForTranscoder", target), nil
	}), nil
}
func rewardCallerActions(target common.Address) []action {
	return resolvedActions(func(ctx context.Context, c *eth.Contracts, from common.Address) ([]action, error) {
		s, err := c.Inspect(ctx)
		if err != nil {
			return nil, err
		}
		caller, err := s.Call(ctx, "bondingManager", "transcoderToRewardCaller", from)
		if err != nil {
			return nil, err
		}
		if caller[0].(common.Address) == target {
			return nil, nil
		}
		return contractAction("bondingManager", "setRewardCaller", target), nil
	})
}
func (p RewardCallerParams) actions() ([]action, error) {
	if p.Address == (common.Address{}) {
		return nil, errors.New("reward caller must be nonempty; use unset")
	}
	return rewardCallerActions(p.Address), nil
}
func (p PollVoteParams) actions() ([]action, error) {
	if p.Address == (common.Address{}) {
		return nil, errors.New("poll address must be nonempty")
	}
	choice := int64(0)
	if p.Choice == "no" {
		choice = 1
	} else if p.Choice != "yes" {
		return nil, errors.New("poll vote must be yes or no")
	}
	result := contractAction("poll", "vote", big.NewInt(choice))
	result[0].Address = &p.Address
	return result, nil
}
func (p ProposalVoteParams) actions() ([]action, error) {
	choices := map[string]uint8{"against": 0, "for": 1, "abstain": 2}
	choice, ok := choices[p.Choice]
	if !ok {
		return nil, errors.New("proposal vote must be against, for or abstain")
	}
	if p.Reason != nil {
		return contractAction("governor", "castVoteWithReason", p.ID.ToBig(), choice, *p.Reason), nil
	}
	return contractAction("governor", "castVote", p.ID.ToBig(), choice), nil
}
