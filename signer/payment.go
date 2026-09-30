package signer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/pm/wire"
)

type remoteSender struct {
	Signer pm.TicketSigner
	Chain  pm.SenderChain
	Policy pm.SenderPolicy
}
type paymentDraft struct {
	Payment, SegCreds string
	State             paymentState
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
	SenderNonce          uint32
	Balance              string
	InitialPricePerUnit  int64
	InitialPixelsPerUnit int64
	Type                 string
	SequenceNumber       uint64
	AuthID               string
	ManifestID           string
}

func (s remoteSender) Generate(ctx context.Context, paymentType, manifest string, info wire.OrchestratorInfo, state paymentState, oldSequence int64) (paymentDraft, error) {
	if s.Signer == nil || (paymentType != "live" && paymentType != "fixed") || manifest == "" || oldSequence < -1 || oldSequence == math.MaxInt64 || info.Price.PricePerUnit <= 0 || info.Price.UnitsPerPrice <= 0 {
		return paymentDraft{}, errors.New("invalid remote payment request")
	}
	now := time.Now().UTC()
	seconds := int64(1)
	if paymentType == "live" {
		if oldSequence < 0 {
			seconds = 10
		} else {
			seconds = max(int64(math.Ceil(now.Sub(state.LastUpdate).Seconds())), 1)
		}
		if seconds > 3600 {
			return paymentDraft{}, errors.New("payment interval exceeds one hour")
		}
	}
	fee := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(info.Price.PricePerUnit), big.NewInt(seconds)), big.NewInt(info.Price.UnitsPerPrice))
	balance := new(big.Rat)
	if state.Balance != "" {
		if _, ok := balance.SetString(state.Balance); !ok {
			return paymentDraft{}, errors.New("invalid balance in payment state")
		}
	}
	params := pm.TicketParams{Recipient: ethcommon.BytesToAddress(info.TicketParams.Recipient), FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), Seed: new(big.Int).SetBytes(info.TicketParams.Seed), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &pm.TicketExpirationParams{CreationRound: info.TicketParams.Expiration.CreationRound, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
	if s.Chain == nil {
		return paymentDraft{}, pm.ErrSenderUnavailable
	}
	funds, err := s.Chain.SenderInfo(ctx, s.Signer.Address(), params.Recipient)
	if err != nil {
		return paymentDraft{}, fmt.Errorf("%w: chain observation failed", pm.ErrSenderUnavailable)
	}
	if funds.Snapshot.Block == nil || funds.Snapshot.Round == nil || params.ExpirationBlock.Cmp(new(big.Int).Add(funds.Snapshot.Block, big.NewInt(1))) <= 0 || params.ExpirationParams.CreationRound < funds.Snapshot.Round.Int64()-2 || params.ExpirationParams.CreationRound > funds.Snapshot.Round.Int64() {
		return paymentDraft{}, pm.ErrRefreshRequired
	}
	if state.PMSessionID != params.RecipientRandHash.Hex() {
		state.SenderNonce = 0
		state.PMSessionID = params.RecipientRandHash.Hex()
	}
	if state.SenderNonce >= 500 {
		return paymentDraft{}, pm.ErrRefreshRequired
	}
	count, err := pm.RemoteBatchSize(params, fee, balance)
	if err != nil {
		return paymentDraft{}, err
	}
	if err := s.Policy.Check(params, count, funds); err != nil {
		return paymentDraft{}, err
	}
	batch, remaining, err := pm.MakeRemoteBatch(params, s.Signer, state.SenderNonce, fee, balance)
	if err != nil {
		return paymentDraft{}, err
	}
	senderParams := make([]wire.TicketSenderParams, 0, len(batch.SenderParams))
	for _, sp := range batch.SenderParams {
		senderParams = append(senderParams, wire.TicketSenderParams{SenderNonce: sp.SenderNonce, Sig: sp.Sig})
	}
	message := wire.Payment{TicketParams: info.TicketParams, Sender: s.Signer.Address().Bytes(), Expiration: info.TicketParams.Expiration, SenderParams: senderParams, ExpectedPrice: info.Price}
	segHash := crypto.Keccak256(nil)
	flatten := append([]byte(manifest), make([]byte, 32)...)
	flatten = append(flatten, segHash...)
	sig, err := s.Signer.SignMessage(flatten)
	if err != nil {
		return paymentDraft{}, err
	}
	segment := wire.SegData{ManifestID: []byte(manifest), Hash: segHash, Signature: sig, Auth: info.Auth}
	state.SenderNonce = batch.SenderParams[len(batch.SenderParams)-1].SenderNonce
	state.Balance = remaining.RatString()
	state.LastUpdate = now
	state.SequenceNumber = uint64(oldSequence + 1)
	return paymentDraft{Payment: base64.StdEncoding.EncodeToString(wire.EncodePayment(message)), SegCreds: base64.StdEncoding.EncodeToString(wire.EncodeSegData(segment)), State: state}, nil
}
