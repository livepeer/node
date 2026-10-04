package orchestrator

import (
	"crypto/hmac"
	"crypto/sha256"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/pm/wire"
)

// Ported from go-livepeer/pm/recipient.go at bd645a09266833fb859053445d9ac85846330756.
// Originally by Yondon Fu, with parameter authentication by Nico Vergauwen:
// 3159761196f5cafca93cdd6b6b40bead5e4ce80c and 5933bc2cfd57a54799d355aa7c78aa1094074c42.
// Preserve the HMAC inputs and their encoding: this derives the private lottery
// value and authenticates the advertised parameters without persisting them.
func (e *PaymentEngine) recipientRand(seed *big.Int, payer ethcommon.Address, faceValue *big.Int, winProb *big.Int, expirationBlock *big.Int, price *big.Rat, ticketExpirationParams *pm.TicketExpirationParams) *big.Int {
	h := hmac.New(sha256.New, e.secret[:])
	msg := append(seed.Bytes(), payer.Bytes()...)
	msg = append(msg, faceValue.Bytes()...)
	msg = append(msg, winProb.Bytes()...)
	msg = append(msg, expirationBlock.Bytes()...)
	msg = append(msg, price.Num().Bytes()...)
	msg = append(msg, price.Denom().Bytes()...)
	msg = append(msg, ticketExpirationParams.AuxData()...)
	h.Write(msg)
	return new(big.Int).SetBytes(h.Sum(nil))
}

// Ported from go-livepeer/core/orchestrator.go's AuthToken. Its key is independent
// of the PM recipient secret and remains in memory for this engine's lifetime.
// Originally by Yondon Fu (fae55ea07c327ddd4bac1c3b5ab4d35c375a4772).
func (e *PaymentEngine) authToken(sessionID string, expiration int64) wire.AuthToken {
	h := hmac.New(sha256.New, e.authSecret[:])
	msg := append([]byte(sessionID), new(big.Int).SetInt64(expiration).Bytes()...)
	h.Write(msg)
	return wire.AuthToken{Token: h.Sum(nil), SessionID: sessionID, Expiration: expiration}
}
