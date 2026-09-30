package signer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/signercompat"
)

type paymentState struct {
	StateID              string
	PMSessionID          string
	LastUpdate           time.Time
	OrchestratorAddress  ethcommon.Address
	App                  string
	AuthExpiry           int64
	SenderNonce          uint32
	Balance              string
	InitialPricePerUnit  int64
	InitialPixelsPerUnit int64
	Type                 string
	SequenceNumber       uint64
	AuthID               string
	ManifestID           string
}

func (s *Service) makePayment(ctx context.Context, req paymentRequest, info signercompat.OrchestratorInfo, state paymentState, oldSequence int64) ([]byte, error) {
	now := time.Now().UTC()
	seconds := int64(1)
	if req.Type == "live" {
		if oldSequence < 0 {
			seconds = 10
		} else {
			seconds = int64(math.Ceil(now.Sub(state.LastUpdate).Seconds()))
			if seconds < 1 {
				seconds = 1
			}
		}
		if seconds > 3600 {
			return nil, invalid("payment interval exceeds one hour")
		}
	}
	fee := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(info.Price.PricePerUnit), big.NewInt(seconds)), big.NewInt(info.Price.UnitsPerPrice))
	balance := new(big.Rat)
	if state.Balance != "" {
		if _, ok := balance.SetString(state.Balance); !ok {
			return nil, invalid("invalid balance in payment state")
		}
	}
	params := pm.TicketParams{Recipient: ethcommon.BytesToAddress(info.TicketParams.Recipient), FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), Seed: new(big.Int).SetBytes(info.TicketParams.Seed), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &pm.TicketExpirationParams{CreationRound: info.TicketParams.Expiration.CreationRound, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
	if s.paymentChain == nil {
		return nil, paymentFailure{482, "sender chain unavailable"}
	}
	funds, err := s.paymentChain.SenderInfo(ctx, s.key.Address(), params.Recipient)
	if err != nil {
		return nil, paymentFailure{482, "sender chain observation failed"}
	}
	if funds.Snapshot.Block == nil || funds.Snapshot.Round == nil || params.ExpirationBlock.Cmp(new(big.Int).Add(funds.Snapshot.Block, big.NewInt(1))) <= 0 || params.ExpirationParams.CreationRound < funds.Snapshot.Round.Int64()-2 || params.ExpirationParams.CreationRound > funds.Snapshot.Round.Int64() {
		return nil, paymentFailure{480, "refresh session for remote signer"}
	}
	count, err := pm.RemoteBatchSize(params, fee, balance)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if err := s.senderPolicy.Check(params, count, funds); err != nil {
		return nil, invalid(err.Error())
	}

	if state.PMSessionID != params.RecipientRandHash.Hex() {
		state.SenderNonce = 0
		state.PMSessionID = params.RecipientRandHash.Hex()
	}
	if state.SenderNonce >= 500 {
		return nil, paymentFailure{480, "refresh session for remote signer"}
	}
	batch, remaining, err := pm.MakeRemoteBatch(params, s.key, state.SenderNonce, fee, balance)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "no tickets"):
			return nil, paymentFailure{482, "no tickets"}
		case strings.Contains(err.Error(), "refresh"):
			return nil, paymentFailure{480, "refresh session for remote signer"}
		default:
			return nil, invalid(err.Error())
		}
	}
	senderParams := make([]signercompat.TicketSenderParams, 0, len(batch.SenderParams))
	for _, sp := range batch.SenderParams {
		senderParams = append(senderParams, signercompat.TicketSenderParams{SenderNonce: sp.SenderNonce, Sig: sp.Sig})
	}
	message := signercompat.Payment{TicketParams: info.TicketParams, Sender: s.key.Address().Bytes(), Expiration: info.TicketParams.Expiration, SenderParams: senderParams, ExpectedPrice: info.Price}
	segHash := crypto.Keccak256(nil)
	flatten := append([]byte(req.ManifestID), make([]byte, 32)...)
	flatten = append(flatten, segHash...)
	sig, err := s.key.SignMessage(flatten)
	if err != nil {
		return nil, err
	}
	segment := signercompat.SegData{ManifestID: []byte(req.ManifestID), Hash: segHash, Signature: sig, Auth: info.Auth}
	state.SenderNonce = batch.SenderParams[len(batch.SenderParams)-1].SenderNonce
	state.Balance = remaining.RatString()
	state.LastUpdate = now
	state.AuthExpiry = info.Auth.Expiration
	state.SequenceNumber = uint64(oldSequence + 1)
	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	stateSig, err := s.key.SignMessage(stateBytes)
	if err != nil {
		return nil, err
	}
	return json.Marshal(paymentResponse{Payment: base64.StdEncoding.EncodeToString(signercompat.EncodePayment(message)), SegCreds: base64.StdEncoding.EncodeToString(signercompat.EncodeSegData(segment)), State: signedState{State: stateBytes, Sig: stateSig}})
}
