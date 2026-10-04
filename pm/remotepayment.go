package pm

import (
	"errors"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

type TicketSigner interface {
	Address() ethcommon.Address
	SignMessage([]byte) ([]byte, error)
}

// RemoteBatchSize retains the legacy minimum-credit floor of one ticket EV.
// The fee is still debited from the balance after the tickets are created.
// Adapted from Yondon Fu's go-livepeer/server/segment_rpc.go newBalanceUpdate
// (5e31f8db3c1d06b40a344927c1cddfc33585c7f7) and core/accounting.go StageUpdate
// (85bdacc93774bf0a61887623802f79e0cfd1f85e).
func RemoteBatchSize(params TicketParams, fee, balance *big.Rat) (int, error) {
	if params.FaceValue == nil || params.WinProb == nil || fee == nil || balance == nil ||
		params.FaceValue.Sign() <= 0 || params.WinProb.Sign() <= 0 || params.WinProb.Cmp(maxWinProb) >= 0 || fee.Sign() <= 0 || balance.Sign() < 0 {
		return 0, errors.New("invalid remote payment values")
	}
	ev := ticketEV(params.FaceValue, params.WinProb)
	if ev.Sign() <= 0 {
		return 0, errors.New("ticket expected value is zero")
	}
	minimumCredit := new(big.Rat).Set(fee)
	if ev.Cmp(minimumCredit) > 0 {
		minimumCredit.Set(ev)
	}
	shortfall := new(big.Rat).Sub(minimumCredit, balance)
	if shortfall.Sign() <= 0 {
		return 0, ErrNoTickets
	}
	countRat := new(big.Rat).Quo(shortfall, ev)
	count, _ := new(big.Int).Divide(countRat.Num(), countRat.Denom(), nil, big.Ceil)
	if !count.IsInt64() || count.Int64() > 100 {
		return 0, errors.New("ticket batch exceeds 100")
	}
	return int(count.Int64()), nil
}

// DraftRemoteBatch constructs unsigned tickets for the billable fee and carries
// forward unused expected value. The caller must keep params unchanged until signing.
// Ticket construction follows Yondon Fu's go-livepeer/pm/sender.go CreateTicketBatch
// (a5ffbb1d31fba67077d5564c1b5161ba900e0be6), used by Josh Allmann's remote signer
// in server/remote_signer.go (f117b4423f8614c7475f0bb0c5071433c7037b87).
func DraftRemoteBatch(params TicketParams, payer ethcommon.Address, firstNonce uint32, fee, balance *big.Rat) (*TicketBatch, *big.Rat, error) {
	if params.FaceValue == nil || params.WinProb == nil || params.ExpirationParams == nil || params.ExpirationBlock == nil || fee == nil || balance == nil {
		return nil, nil, errors.New("incomplete remote payment params")
	}
	count, err := RemoteBatchSize(params, fee, balance)
	if err != nil {
		return nil, nil, err
	}
	ev := ticketEV(params.FaceValue, params.WinProb)
	if uint64(firstNonce)+uint64(count) >= 600 {
		return nil, nil, ErrRefreshRequired
	}
	batch := &TicketBatch{TicketParams: &params, TicketExpirationParams: params.ExpirationParams, PayerAddress: payer}
	for i := 0; i < count; i++ {
		nonce := firstNonce + uint32(i) + 1
		batch.PayerParams = append(batch.PayerParams, &TicketPayerParams{TicketNonce: nonce})
	}
	credit := new(big.Rat).Mul(ev, big.NewRat(int64(count), 1))
	remaining := new(big.Rat).Sub(new(big.Rat).Add(balance, credit), fee)
	return batch, remaining, nil
}
