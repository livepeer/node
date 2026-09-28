package pm

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/signercompat"
)

type ChainSnapshot struct {
	Block     *big.Int
	Round     *big.Int
	RoundHash ethcommon.Hash
}

// PaymentChain contains only the Ethereum reads needed by payment receipt.
type PaymentChain interface {
	Snapshot(context.Context) (ChainSnapshot, error)
	IsActive(context.Context, ethcommon.Address) (bool, error)
	ValidateSender(context.Context, ethcommon.Address, *big.Int) error
}

type Engine struct {
	mu        sync.Mutex
	store     *SQLiteStore
	chain     PaymentChain
	recipient ethcommon.Address
	faceValue *big.Int
	winProb   *big.Int
}

func NewEngine(store *SQLiteStore, chain PaymentChain, recipient ethcommon.Address, faceValue, winProb *big.Int) (*Engine, error) {
	if store == nil || chain == nil || recipient == (ethcommon.Address{}) || faceValue == nil || faceValue.Sign() <= 0 || winProb == nil || winProb.Sign() <= 0 || winProb.Cmp(maxWinProb) > 0 {
		return nil, errors.New("invalid payment engine configuration")
	}
	return &Engine{store: store, chain: chain, recipient: recipient, faceValue: new(big.Int).Set(faceValue), winProb: new(big.Int).Set(winProb)}, nil
}

type Challenge struct {
	PaymentParams string `json:"payment_params"`
	Orchestrator  string `json:"orchestrator"`
	ManifestID    string `json:"manifest_id"`
	PaymentURL    string `json:"payment_url"`
}

func randomBytes(length int) ([]byte, error) {
	b := make([]byte, length)
	_, err := rand.Read(b)
	return b, err
}

// MakeChallenge pins the runner, sender, price, auth token and ticket params in
// SQLite before returning a Python/Go runner compatible 402 body.
func (e *Engine) MakeChallenge(ctx context.Context, runner, manifest string, sender ethcommon.Address, price int64, unit, service string) (Challenge, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if runner == "" || manifest == "" || sender == (ethcommon.Address{}) || price <= 0 || (unit != "seconds" && unit != "fixed") {
		return Challenge{}, errors.New("invalid payment challenge scope")
	}
	snapshot, err := e.chain.Snapshot(ctx)
	if err != nil {
		return Challenge{}, err
	}
	active, err := e.chain.IsActive(ctx, e.recipient)
	if err != nil {
		return Challenge{}, err
	}
	if !active {
		return Challenge{}, errors.New("orchestrator is not active on chain")
	}
	if snapshot.Block == nil || snapshot.Round == nil || snapshot.Block.Sign() < 0 || snapshot.Round.Sign() <= 0 || snapshot.RoundHash == (ethcommon.Hash{}) {
		return Challenge{}, errors.New("chain snapshot unavailable")
	}
	random, err := randomBytes(32)
	if err != nil {
		return Challenge{}, err
	}
	seed, err := randomBytes(32)
	if err != nil {
		return Challenge{}, err
	}
	token, err := randomBytes(32)
	if err != nil {
		return Challenge{}, err
	}
	randHash := crypto.Keccak256Hash(random)
	info := signercompat.OrchestratorInfo{Transcoder: service, Address: e.recipient.Bytes(), Price: signercompat.PriceInfo{PricePerUnit: price, UnitsPerPrice: 1},
		TicketParams: signercompat.TicketParams{Recipient: e.recipient.Bytes(), FaceValue: e.faceValue.Bytes(), WinProb: e.winProb.Bytes(), RecipientRandHash: randHash.Bytes(), Seed: seed, ExpirationBlock: new(big.Int).Add(snapshot.Block, big.NewInt(40)).Bytes(), Expiration: signercompat.ExpirationParams{CreationRound: snapshot.Round.Int64(), CreationRoundBlockHash: snapshot.RoundHash.Bytes()}},
		Auth:         signercompat.AuthToken{Token: token, SessionID: manifest, Expiration: time.Now().Add(time.Hour).Unix()}}
	encoded := signercompat.EncodeOrchestratorInfo(info)
	_, err = e.store.db.Exec(`INSERT INTO payment_challenges(manifest,runner,sender,info,recipient_rand,unit,balance,created_at) VALUES(?,?,?,?,?,?,'0',?)
		ON CONFLICT(manifest) DO UPDATE SET info=excluded.info,recipient_rand=excluded.recipient_rand`, manifest, runner, sender.Hex(), encoded, random, unit, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return Challenge{}, err
	}
	return Challenge{PaymentParams: base64.StdEncoding.EncodeToString(encoded), Orchestrator: service, ManifestID: manifest, PaymentURL: service + "/apps/" + runner + "/session/" + manifest + "/payment"}, nil
}

func (e *Engine) ChallengeInfo(manifest string) (signercompat.OrchestratorInfo, error) {
	var encoded []byte
	if err := e.store.db.QueryRow("SELECT info FROM payment_challenges WHERE manifest=?", manifest).Scan(&encoded); err != nil {
		return signercompat.OrchestratorInfo{}, err
	}
	return signercompat.DecodeOrchestratorInfo(encoded)
}

func (e *Engine) ChallengeForManifest(runner, manifest, service string) (Challenge, error) {
	info, err := e.ChallengeInfo(manifest)
	if err != nil {
		return Challenge{}, err
	}
	return Challenge{PaymentParams: base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), Orchestrator: service, ManifestID: manifest, PaymentURL: service + "/apps/" + runner + "/session/" + manifest + "/payment"}, nil
}

func (e *Engine) RefreshChallenge(ctx context.Context, manifest string, sender ethcommon.Address, service string) (Challenge, error) {
	var runner, senderString, unit string
	var encoded []byte
	if err := e.store.db.QueryRow("SELECT runner,sender,unit,info FROM payment_challenges WHERE manifest=?", manifest).Scan(&runner, &senderString, &unit, &encoded); err != nil {
		return Challenge{}, err
	}
	if sender.Hex() != senderString {
		return Challenge{}, ErrInvalidPayment
	}
	info, err := signercompat.DecodeOrchestratorInfo(encoded)
	if err != nil {
		return Challenge{}, err
	}
	return e.MakeChallenge(ctx, runner, manifest, sender, info.Price.PricePerUnit, unit, service)
}

func ManifestFromSegment(header string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return "", ErrInvalidPayment
	}
	segment, err := signercompat.DecodeSegData(data)
	if err != nil || segment.Auth.SessionID == "" || string(segment.ManifestID) != segment.Auth.SessionID {
		return "", ErrInvalidPayment
	}
	return segment.Auth.SessionID, nil
}

var ErrInvalidPayment = errors.New("invalid payment")
var ErrInsufficientBalance = errors.New("insufficient payment balance")
var ErrMissingChallenge = errors.New("payment challenge not found")
var ErrFixedAlreadyCharged = errors.New("fixed-price session already charged")

func decodeHeaders(paymentHeader, segmentHeader string) (signercompat.Payment, signercompat.SegData, error) {
	paymentBytes, err := base64.StdEncoding.DecodeString(paymentHeader)
	if err != nil {
		return signercompat.Payment{}, signercompat.SegData{}, ErrInvalidPayment
	}
	segmentBytes, err := base64.StdEncoding.DecodeString(segmentHeader)
	if err != nil {
		return signercompat.Payment{}, signercompat.SegData{}, ErrInvalidPayment
	}
	payment, err := signercompat.DecodePayment(paymentBytes)
	if err != nil {
		return signercompat.Payment{}, signercompat.SegData{}, ErrInvalidPayment
	}
	segment, err := signercompat.DecodeSegData(segmentBytes)
	if err != nil {
		return signercompat.Payment{}, signercompat.SegData{}, ErrInvalidPayment
	}
	return payment, segment, nil
}

func (e *Engine) Receive(ctx context.Context, runner, manifest, paymentHeader, segmentHeader string) (ethcommon.Address, *big.Rat, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	payment, segment, err := decodeHeaders(paymentHeader, segmentHeader)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	if len(payment.Sender) != 20 || len(segment.Hash) != 32 || string(segment.ManifestID) != manifest || segment.Auth.SessionID != manifest || len(segment.Signature) != 65 {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	var storedRunner, senderString string
	var encoded, recipientRand []byte
	err = e.store.db.QueryRow("SELECT runner,sender,info,recipient_rand FROM payment_challenges WHERE manifest=?", manifest).Scan(&storedRunner, &senderString, &encoded, &recipientRand)
	if errors.Is(err, sql.ErrNoRows) {
		return ethcommon.Address{}, nil, ErrMissingChallenge
	}
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	info, err := signercompat.DecodeOrchestratorInfo(encoded)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	sender := ethcommon.BytesToAddress(payment.Sender)
	if storedRunner != runner || sender.Hex() != senderString || !bytes.Equal(segment.Auth.Token, info.Auth.Token) || segment.Auth.Expiration != info.Auth.Expiration || time.Now().Unix() > info.Auth.Expiration ||
		!bytes.Equal(signercompat.EncodeTicketParams(payment.TicketParams), signercompat.EncodeTicketParams(info.TicketParams)) ||
		payment.Expiration.CreationRound != info.TicketParams.Expiration.CreationRound || !bytes.Equal(payment.Expiration.CreationRoundBlockHash, info.TicketParams.Expiration.CreationRoundBlockHash) ||
		payment.ExpectedPrice != info.Price || len(payment.SenderParams) == 0 || len(payment.SenderParams) > 100 {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	flatten := append([]byte(manifest), make([]byte, 32)...)
	flatten = append(flatten, segment.Hash...)
	if !(DefaultSigVerifier{}).Verify(sender, flatten, segment.Signature) {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	snapshot, err := e.chain.Snapshot(ctx)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	params := TicketParams{Recipient: e.recipient, FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), Seed: new(big.Int).SetBytes(info.TicketParams.Seed), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &TicketExpirationParams{CreationRound: info.TicketParams.Expiration.CreationRound, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
	if params.ExpirationBlock.Cmp(snapshot.Block) <= 0 || params.FaceValue.Sign() <= 0 || params.WinProb.Sign() <= 0 {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	if err := e.chain.ValidateSender(ctx, sender, params.FaceValue); err != nil {
		return ethcommon.Address{}, nil, err
	}
	// A single transaction atomically records nonce replay protection, winning
	// tickets and expected-value credit for the session.
	tx, err := e.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	defer tx.Rollback()
	var currentBalance string
	if err := tx.QueryRow("SELECT balance FROM payment_challenges WHERE manifest=?", manifest).Scan(&currentBalance); err != nil {
		return ethcommon.Address{}, nil, err
	}
	balance, ok := new(big.Rat).SetString(currentBalance)
	if !ok {
		return ethcommon.Address{}, nil, errors.New("corrupt payment balance")
	}
	validator := NewValidator(DefaultSigVerifier{}, nil)
	winningRand := new(big.Int).SetBytes(recipientRand)
	for _, sp := range payment.SenderParams {
		if sp.SenderNonce == 0 || sp.SenderNonce >= 600 || len(sp.Sig) != 65 {
			return ethcommon.Address{}, nil, ErrInvalidPayment
		}
		ticket := NewTicket(&params, params.ExpirationParams, sender, sp.SenderNonce)
		if err := validator.ValidateTicket(e.recipient, ticket, sp.Sig, winningRand); err != nil {
			return ethcommon.Address{}, nil, ErrInvalidPayment
		}
		if _, err := tx.Exec("INSERT INTO used_payment_tickets(sender,recipient_rand_hash,sender_nonce) VALUES(?,?,?)", sender.Hex(), params.RecipientRandHash.Hex(), sp.SenderNonce); err != nil {
			return ethcommon.Address{}, nil, ErrInvalidPayment
		}
		balance.Add(balance, ticket.EV())
		if validator.IsWinningTicket(ticket, sp.Sig, winningRand) {
			_, err := tx.Exec(`INSERT INTO winning_tickets(sender,recipient,face_value,win_prob,sender_nonce,recipient_rand,recipient_rand_hash,sig,creation_round,creation_round_block_hash,params_expiration_block)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, sender.Hex(), ticket.Recipient.Hex(), ticket.FaceValue.Bytes(), ticket.WinProb.Bytes(), ticket.SenderNonce, recipientRand, ticket.RecipientRandHash.Hex(), sp.Sig, ticket.CreationRound, ticket.CreationRoundBlockHash.Hex(), ticket.ParamsExpirationBlock.String())
			if err != nil {
				return ethcommon.Address{}, nil, err
			}
		}
	}
	if _, err := tx.Exec("UPDATE payment_challenges SET balance=? WHERE manifest=?", balance.RatString(), manifest); err != nil {
		return ethcommon.Address{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return ethcommon.Address{}, nil, err
	}
	return sender, balance, nil
}

func (e *Engine) Charge(ctx context.Context, manifest string, now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, err := e.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var encoded []byte
	var unit, balanceString string
	var lastCharge sql.NullString
	if err := tx.QueryRow("SELECT info,unit,balance,last_charge FROM payment_challenges WHERE manifest=?", manifest).Scan(&encoded, &unit, &balanceString, &lastCharge); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMissingChallenge
		}
		return err
	}
	info, err := signercompat.DecodeOrchestratorInfo(encoded)
	if err != nil {
		return err
	}
	balance, ok := new(big.Rat).SetString(balanceString)
	if !ok {
		return errors.New("corrupt payment balance")
	}
	units := int64(0)
	if unit == "fixed" {
		if lastCharge.Valid {
			return ErrFixedAlreadyCharged
		}
		units = 1
	} else if unit == "seconds" && lastCharge.Valid {
		previous, err := time.Parse(time.RFC3339Nano, lastCharge.String)
		if err != nil {
			return err
		}
		units = int64(now.Sub(previous) / time.Second)
		if units < 0 {
			units = 0
		}
		if units > 3600 {
			return ErrInsufficientBalance
		}
		if units == 0 {
			return tx.Commit()
		}
		now = previous.Add(time.Duration(units) * time.Second)
	} else if unit != "seconds" {
		return errors.New("invalid payment unit")
	}
	fee := new(big.Rat).Mul(big.NewRat(info.Price.PricePerUnit, info.Price.UnitsPerPrice), big.NewRat(units, 1))
	if balance.Cmp(fee) < 0 {
		return ErrInsufficientBalance
	}
	balance.Sub(balance, fee)
	_, err = tx.Exec("UPDATE payment_challenges SET balance=?,last_charge=? WHERE manifest=?", balance.RatString(), now.UTC().Format(time.RFC3339Nano), manifest)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) Balance(manifest string) (*big.Rat, error) {
	var value string
	if err := e.store.db.QueryRow("SELECT balance FROM payment_challenges WHERE manifest=?", manifest).Scan(&value); err != nil {
		return nil, err
	}
	result, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("corrupt balance")
	}
	return result, nil
}
