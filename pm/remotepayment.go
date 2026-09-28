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

// MakeRemoteBatch constructs signed tickets for an exact billable fee and
// carries forward the unused expected value. Values stay rational until the
// final comparison, matching go-livepeer's probabilistic payment accounting.
func MakeRemoteBatch(params TicketParams, signer TicketSigner, firstNonce uint32, fee, balance *big.Rat) (*TicketBatch, *big.Rat, error) {
	if signer == nil || params.FaceValue == nil || params.WinProb == nil || params.ExpirationParams == nil || params.ExpirationBlock == nil || fee == nil || balance == nil {
		return nil, nil, errors.New("incomplete remote payment params")
	}
	if params.FaceValue.Sign() <= 0 || params.WinProb.Sign() <= 0 || params.WinProb.Cmp(maxWinProb) > 0 || fee.Sign() <= 0 || balance.Sign() < 0 {
		return nil, nil, errors.New("invalid remote payment values")
	}
	ev := ticketEV(params.FaceValue, params.WinProb)
	if ev.Sign() <= 0 {
		return nil, nil, errors.New("ticket expected value is zero")
	}
	shortfall := new(big.Rat).Sub(fee, balance)
	if shortfall.Sign() <= 0 {
		return nil, nil, errors.New("no tickets needed")
	}
	countRat := new(big.Rat).Quo(shortfall, ev)
	count := new(big.Int).Quo(countRat.Num(), countRat.Denom())
	if new(big.Int).Rem(countRat.Num(), countRat.Denom()).Sign() != 0 {
		count.Add(count, big.NewInt(1))
	}
	if !count.IsInt64() || count.Int64() > 100 {
		return nil, nil, errors.New("ticket batch exceeds 100")
	}
	if uint64(firstNonce)+uint64(count.Int64()) >= 600 {
		return nil, nil, errors.New("ticket nonce requires session refresh")
	}
	batch := &TicketBatch{TicketParams: &params, TicketExpirationParams: params.ExpirationParams, Sender: signer.Address()}
	for i := int64(0); i < count.Int64(); i++ {
		nonce := firstNonce + uint32(i) + 1
		ticket := NewTicket(&params, params.ExpirationParams, signer.Address(), nonce)
		sig, err := signer.SignMessage(ticket.Hash().Bytes())
		if err != nil {
			return nil, nil, err
		}
		batch.SenderParams = append(batch.SenderParams, &TicketSenderParams{SenderNonce: nonce, Sig: sig})
	}
	credit := new(big.Rat).Mul(ev, new(big.Rat).SetInt(count))
	remaining := new(big.Rat).Sub(new(big.Rat).Add(balance, credit), fee)
	return batch, remaining, nil
}
