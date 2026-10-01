package pm

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/livepeer/node/eth"
)

// SenderPolicy limits actual ticket exposure, independently of the work price.
// Defaults match the retained go-livepeer sender policy (values are in wei).
// Adapted from pm/sender.go's validation by Yondon Fu and Nico Vergauwen,
// including Rafał Leszko's batch EV limit (366c2d68b39143f7b805c1aa98a0da069104134e).
type SenderPolicy struct {
	MaxTicketEV, MaxBatchEV *big.Rat
	DepositMultiplier       int64
}

func DefaultSenderPolicy() SenderPolicy {
	return SenderPolicy{big.NewRat(3_000_000_000_000, 1), big.NewRat(20_000_000_000_000, 1), 1}
}

func (p SenderPolicy) Validate() error {
	if p.MaxTicketEV == nil || p.MaxTicketEV.Sign() <= 0 || p.MaxBatchEV == nil || p.MaxBatchEV.Sign() <= 0 || p.DepositMultiplier < 1 {
		return errors.New("ticket and batch EV limits and deposit multiplier must be positive")
	}
	return nil
}

func ValidateSenderFunds(info eth.SenderInfo) error {
	if info.Deposit == nil || info.Reserve == nil || info.WithdrawRound == nil || info.Snapshot.Round == nil || info.Deposit.Sign() <= 0 || info.Reserve.Sign() <= 0 {
		return errors.New("sender deposit or claimable reserve is unavailable")
	}
	if info.WithdrawRound.Sign() != 0 && info.WithdrawRound.Cmp(new(big.Int).Add(info.Snapshot.Round, big.NewInt(1))) <= 0 {
		return errors.New("sender deposit and reserve unlock too soon")
	}
	return nil
}

func (p SenderPolicy) Check(params TicketParams, count int, funds eth.SenderInfo) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := ValidateSenderFunds(funds); err != nil {
		return fmt.Errorf("%w: %v", ErrSenderUnavailable, err)
	}
	if params.FaceValue == nil || params.WinProb == nil || params.FaceValue.Sign() <= 0 || params.WinProb.Sign() <= 0 || params.WinProb.Cmp(maxWinProb) >= 0 || count < 1 || count > 100 {
		return errors.New("invalid ticket exposure")
	}
	ev := ticketEV(params.FaceValue, params.WinProb)
	if ev.Cmp(p.MaxTicketEV) > 0 || new(big.Rat).Mul(ev, big.NewRat(int64(count), 1)).Cmp(p.MaxBatchEV) > 0 {
		return errors.New("ticket expected value exceeds sender policy")
	}
	if params.FaceValue.Cmp(new(big.Int).Quo(funds.Deposit, big.NewInt(p.DepositMultiplier))) > 0 {
		return errors.New("ticket face value exceeds sender deposit policy")
	}
	return nil
}

var (
	ErrNoTickets         = errors.New("no tickets needed")
	ErrRefreshRequired   = errors.New("refresh session for remote signer")
	ErrSenderUnavailable = errors.New("sender funds unavailable")
)
