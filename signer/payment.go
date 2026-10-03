package signer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/pm/wire"
)

const paramsExpiryBuffer = int64(1)

type remotePayer struct {
	Signer pm.TicketSigner
	Chain  pm.PayerChain
	Policy pm.PayerPolicy
}
type paymentDraft struct {
	Payment, SegCreds string
	State             paymentState
	Usage             paymentUsage
}

type paymentUsage struct {
	Fee, Balance *big.Rat
	PreviousTime time.Time
	BillableSecs float64
	NumTickets   int
}

type paymentState struct {
	StateID              string
	PMSessionID          string
	LastUpdate           time.Time
	OrchestratorAddress  ethcommon.Address
	App                  string
	AuthExpiry           int64
	AuthPolicy           string
	AuthMaxPrice         string
	TicketNonce          uint32 `json:"SenderNonce"`
	Balance              string
	InitialPricePerUnit  int64
	InitialPixelsPerUnit int64
	Type                 string
	SequenceNumber       uint64
	AuthID               string
	ManifestID           string
}

func (s remotePayer) Generate(ctx context.Context, paymentType, manifest string, info wire.OrchestratorInfo, state paymentState, oldSequence int64) (paymentDraft, error) {
	if s.Signer == nil || (paymentType != "live" && paymentType != "fixed") || manifest == "" || oldSequence < -1 || oldSequence == math.MaxInt64 || info.Price.PricePerUnit <= 0 || info.Price.UnitsPerPrice <= 0 {
		return paymentDraft{}, invalid("invalid remote payment request")
	}
	now := time.Now().UTC()
	previous := state.LastUpdate.UTC()
	if previous.IsZero() {
		previous = now
	}
	billableSecs := now.Sub(previous).Seconds()
	seconds := int64(1)
	if paymentType == "live" {
		if oldSequence < 0 {
			seconds = 10
			billableSecs = 10
		} else {
			seconds = max(int64(math.Ceil(now.Sub(state.LastUpdate).Seconds())), 1)
		}
		if seconds > 3600 {
			return paymentDraft{}, invalid("payment interval exceeds one hour")
		}
	}
	fee := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(info.Price.PricePerUnit), big.NewInt(seconds)), big.NewInt(info.Price.UnitsPerPrice))
	balance := new(big.Rat)
	if state.Balance != "" {
		if _, ok := balance.SetString(state.Balance); !ok {
			return paymentDraft{}, invalid("invalid balance in payment state")
		}
	}
	params := pm.TicketParams{Recipient: ethcommon.BytesToAddress(info.TicketParams.Recipient), FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), Seed: new(big.Int).SetBytes(info.TicketParams.Seed), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &pm.TicketExpirationParams{CreationRound: info.TicketParams.Expiration.CreationRound, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
	if state.PMSessionID != params.RecipientRandHash.Hex() {
		state.TicketNonce = 0
		state.PMSessionID = params.RecipientRandHash.Hex()
	}
	if state.TicketNonce >= 500 {
		return paymentDraft{}, pm.ErrRefreshRequired
	}
	// Precheck one ticket before generating the batch.
	// Observation/funding failures here are client errors.
	if err := s.validateTicketParams(ctx, &params, 1, "precheck"); err != nil {
		if errors.Is(err, pm.ErrRefreshRequired) {
			return paymentDraft{}, err
		}
		if f, ok := errors.AsType[paymentFailure](err); ok {
			return paymentDraft{}, f
		}
		return paymentDraft{}, invalid("payer funds unavailable")
	}
	count, err := pm.RemoteBatchSize(params, fee, balance)
	if err != nil {
		if errors.Is(err, pm.ErrNoTickets) {
			return paymentDraft{}, err
		}
		slog.WarnContext(ctx, "signer ticket batch rejected", "error", err)
		return paymentDraft{}, invalid("invalid ticket batch")
	}
	// Recheck the full batch before signing. A later observation/funding failure
	// is an internal generation failure; invalid exposure remains a client error.
	if err := s.validateTicketParams(ctx, &params, count, "generation"); err != nil {
		return paymentDraft{}, err
	}
	batch, remaining, err := pm.MakeRemoteBatch(params, s.Signer, state.TicketNonce, fee, balance)
	if err != nil {
		if !errors.Is(err, pm.ErrRefreshRequired) && !errors.Is(err, pm.ErrNoTickets) {
			slog.ErrorContext(ctx, "signer ticket batch generation failed", "error", err)
		}
		return paymentDraft{}, err
	}
	payerParams := make([]wire.TicketPayerParams, 0, len(batch.PayerParams))
	for _, sp := range batch.PayerParams {
		payerParams = append(payerParams, wire.TicketPayerParams{TicketNonce: sp.TicketNonce, Sig: sp.Sig})
	}
	message := wire.Payment{TicketParams: info.TicketParams, PayerAddress: s.Signer.Address().Bytes(), Expiration: info.TicketParams.Expiration, PayerParams: payerParams, ExpectedPrice: info.Price}
	segHash := crypto.Keccak256(nil)
	flatten := append([]byte(manifest), make([]byte, 32)...)
	flatten = append(flatten, segHash...)
	sig, err := s.Signer.SignMessage(flatten)
	if err != nil {
		slog.ErrorContext(ctx, "signer segment credential signing failed", "error", err)
		return paymentDraft{}, err
	}
	segment := wire.SegData{ManifestID: []byte(manifest), Hash: segHash, Signature: sig, Auth: info.Auth}
	state.TicketNonce = batch.PayerParams[len(batch.PayerParams)-1].TicketNonce
	state.Balance = remaining.RatString()
	state.LastUpdate = now
	state.SequenceNumber = uint64(oldSequence + 1)
	return paymentDraft{Payment: base64.StdEncoding.EncodeToString(wire.EncodePayment(message)), SegCreds: base64.StdEncoding.EncodeToString(wire.EncodeSegData(segment)), State: state,
		Usage: paymentUsage{Fee: fee, Balance: remaining, PreviousTime: previous, BillableSecs: billableSecs, NumTickets: len(batch.PayerParams)}}, nil
}

// Adapted from go-livepeer/pm/sender.go's validateTicketParams, by Yondon Fu,
// Nico Vergauwen and Rafał Leszko. Supplied creation-round metadata is retained;
// refresh is governed by parameter expiry on the L1 clock, not the current round.
func (s remotePayer) validateTicketParams(ctx context.Context, ticketParams *pm.TicketParams, numTickets int, phase string) error {
	if ticketParams == nil {
		return invalid("ticketParams is nil")
	}
	if ticketParams.ExpirationBlock.Int64() == 0 {
		return invalid("ticketParams expiration block is 0")
	}
	if s.Chain == nil {
		slog.ErrorContext(ctx, "signer payer chain is not configured", "phase", phase)
		return pm.ErrPayerUnavailable
	}
	funds, err := s.Chain.PayerFunds(ctx, s.Signer.Address(), ticketParams.Recipient)
	if err != nil {
		slog.ErrorContext(ctx, "signer payer chain observation failed", "phase", phase, "error", err)
		return fmt.Errorf("%w: chain observation failed", pm.ErrPayerUnavailable)
	}
	if funds.Snapshot.Block == nil {
		slog.ErrorContext(ctx, "signer payer chain observation has no L1 block", "phase", phase)
		return fmt.Errorf("%w: chain observation failed", pm.ErrPayerUnavailable)
	}
	latestL1Block := funds.Snapshot.Block
	currentBuffer := new(big.Int).Sub(ticketParams.ExpirationBlock, latestL1Block).Int64()
	if currentBuffer <= paramsExpiryBuffer {
		return pm.ErrRefreshRequired
	}
	if err := s.Policy.Check(*ticketParams, numTickets, funds); err != nil {
		slog.WarnContext(ctx, "signer payer policy rejected ticket parameters", "phase", phase, "num_tickets", numTickets, "error", err)
		if errors.Is(err, pm.ErrPayerUnavailable) {
			return err
		}
		return invalid("ticket parameters violate payer policy")
	}
	return nil
}
