package chain

import (
	"errors"
	"math/big"

	"github.com/livepeer/node/destination"
)

func contractAction(contract, method string, args ...any) []action {
	return []action{{Contract: contract, Method: method, Args: args, Value: big.NewInt(0)}}
}

func (p PercentageOptions) transcoderAction() action {
	return contractAction("bondingManager", "transcoder", new(big.Int).SetUint64(*p.RewardCut), new(big.Int).SetUint64(*p.FeeShare))[0]
}

func (p ActivateParams) actions() ([]action, error) {
	return []action{p.transcoderAction()}, nil
}

func (p SetConfigParams) actions() ([]action, error) {
	var result []action
	if p.RewardCut != nil || p.FeeShare != nil {
		if p.RewardCut == nil || p.FeeShare == nil {
			return nil, errors.New("reward-cut and fee-share are required together")
		}
		result = append(result, p.transcoderAction())
	}
	if u := p.ServiceURI.Value; u != nil {
		if err := destination.ValidateURL(u); err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("service-uri must be an absolute HTTP or HTTPS URL")
		}
		result = append(result, contractAction("serviceRegistry", "setServiceURI", u.String())...)
	}
	if len(result) == 0 {
		return nil, errors.New("reward-cut and fee-share or service-uri is required")
	}
	return result, nil
}

func (p BondParams) actions() ([]action, error) {
	return contractAction("bondingManager", "bond", p.Amount.ToBig(), p.Orchestrator), nil
}

func (p UnbondParams) actions() ([]action, error) {
	return contractAction("bondingManager", "unbond", p.Amount.ToBig()), nil
}

func (p RebondParams) actions() ([]action, error) {
	lock := p.LockID.ToBig()
	if p.Delegate != nil {
		return contractAction("bondingManager", "rebondFromUnbonded", *p.Delegate, lock), nil
	}
	return contractAction("bondingManager", "rebond", lock), nil
}

func (p WithdrawStakeParams) actions() ([]action, error) {
	return contractAction("bondingManager", "withdrawStake", p.LockID.ToBig()), nil
}

func (p ClaimParams) actions() ([]action, error) {
	return contractAction("bondingManager", "claimEarnings", p.EndRound.ToBig()), nil
}

func (p WithdrawFeesParams) actions() ([]action, error) {
	return contractAction("bondingManager", "withdrawFees", p.Recipient, p.Amount.ToBig()), nil
}

func (p FundParams) actions() ([]action, error) {
	deposit, reserve := p.Amount.ToBig(), p.Reserve.ToBig()
	value := new(big.Int).Add(deposit, reserve)
	if value.BitLen() > 256 {
		return nil, errors.New("deposit and reserve total exceeds uint256")
	}
	if value.Sign() == 0 {
		return nil, errors.New("deposit and reserve cannot both be zero")
	}
	return []action{{"ticketBroker", "fundDepositAndReserve", []any{deposit, reserve}, value}}, nil
}
