package pm

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"math/big"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm/wire"
)

type ChainSnapshot = eth.ChainSnapshot

// PayerChain contains the payer collateral observation used by both payment sides.
type PayerChain interface {
	PayerFunds(context.Context, ethcommon.Address, ethcommon.Address) (PayerFunds, error)
}

// PaymentChain contains only the Ethereum reads needed by payment receipt.
type PaymentChain interface {
	Snapshot(context.Context) (ChainSnapshot, error)
	IsActiveAt(context.Context, ethcommon.Address, ChainSnapshot) (bool, error)
	PayerChain
}

type Engine struct {
	mu            sync.Mutex
	store         *SQLiteStore
	chain         PaymentChain
	recipient     ethcommon.Address
	faceValue     *big.Int
	winProb       *big.Int
	secret        [32]byte
	authSecret    [32]byte
	sessions      map[string]*paymentSession
	ticketNonces  map[string]*recipientNonces
	nonceCount    int
	lastSeenBlock *big.Int
}

// Session scope, the latest response, and accounting are process-local, just as
// the runner sessions are. Issued PM parameters and secrets are never persisted.
// The cached response serves refresh/retry routes; it does not authenticate PM.
type paymentSession struct {
	runner     string
	payer      ethcommon.Address
	price      wire.PriceInfo
	unit       string
	info       []byte
	balance    *big.Rat
	lastCharge time.Time
	updated    time.Time
}

func NewEngine(store *SQLiteStore, chain PaymentChain, recipient ethcommon.Address, faceValue, winProb *big.Int) (*Engine, error) {
	if store == nil || chain == nil || recipient == (ethcommon.Address{}) || faceValue == nil || faceValue.Sign() <= 0 || winProb == nil || winProb.Sign() <= 0 || winProb.Cmp(maxWinProb) >= 0 {
		return nil, errors.New("invalid payment engine configuration")
	}
	e := &Engine{store: store, chain: chain, recipient: recipient, faceValue: new(big.Int).Set(faceValue), winProb: new(big.Int).Set(winProb), sessions: make(map[string]*paymentSession), ticketNonces: make(map[string]*recipientNonces), lastSeenBlock: new(big.Int)}
	// Port of Yondon Fu's go-livepeer/pm.NewRecipient: a fresh 256-bit HMAC
	// key per recipient lifetime. AuthToken has its own independent key, as upstream.
	rand.Read(e.secret[:])
	rand.Read(e.authSecret[:])
	return e, nil
}

type Challenge struct {
	PaymentParams string `json:"payment_params"`
	Orchestrator  string `json:"orchestrator"`
	ManifestID    string `json:"manifest_id"`
	PaymentURL    string `json:"payment_url"`
}

// MakeChallenge returns a Python/Go runner compatible 402 body. Ticket
// parameters are authenticated by the recipient HMAC, as in go-livepeer.
func (e *Engine) MakeChallenge(ctx context.Context, runner, manifest string, payer ethcommon.Address, price int64, unit, service string) (Challenge, error) {
	if runner == "" || manifest == "" || payer == (ethcommon.Address{}) || price <= 0 || (unit != "seconds" && unit != "fixed") {
		return Challenge{}, errors.New("invalid payment challenge scope")
	}
	snapshot, err := e.chain.Snapshot(ctx)
	if err != nil {
		return Challenge{}, err
	}
	active, err := e.chain.IsActiveAt(ctx, e.recipient, snapshot)
	if err != nil {
		return Challenge{}, err
	}
	if !active {
		return Challenge{}, errors.New("orchestrator is not active on chain")
	}
	if snapshot.Block == nil || snapshot.Round == nil || snapshot.Block.Sign() < 0 || snapshot.Round.Sign() <= 0 || snapshot.RoundHash == (ethcommon.Hash{}) {
		return Challenge{}, errors.New("chain snapshot unavailable")
	}
	seed := make([]byte, 32)
	rand.Read(seed)
	priceInfo := wire.PriceInfo{PricePerUnit: price, UnitsPerPrice: 1}
	expiration := &TicketExpirationParams{CreationRound: snapshot.Round.Int64(), CreationRoundBlockHash: snapshot.RoundHash}
	expiresAtBlock := new(big.Int).Add(snapshot.Block, big.NewInt(40))
	recipientRand := e.recipientRand(new(big.Int).SetBytes(seed), payer, e.faceValue, e.winProb, expiresAtBlock, big.NewRat(price, 1), expiration)
	randHash := crypto.Keccak256Hash(ethcommon.LeftPadBytes(recipientRand.Bytes(), uint256Size))
	info := wire.OrchestratorInfo{Transcoder: service, Address: e.recipient.Bytes(), Price: priceInfo,
		TicketParams: wire.TicketParams{Recipient: e.recipient.Bytes(), FaceValue: e.faceValue.Bytes(), WinProb: e.winProb.Bytes(), RecipientRandHash: randHash.Bytes(), Seed: seed, ExpirationBlock: expiresAtBlock.Bytes(), Expiration: wire.ExpirationParams{CreationRound: expiration.CreationRound, CreationRoundBlockHash: expiration.CreationRoundBlockHash.Bytes()}},
		Auth:         e.authToken(manifest, time.Now().Add(time.Hour).Unix())}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observeBlock(snapshot.Block)
	current := e.sessions[manifest]
	if current == nil {
		if len(e.sessions) >= 100000 {
			return Challenge{}, errors.New("payment session capacity reached")
		}
		current = &paymentSession{runner: runner, payer: payer, price: priceInfo, unit: unit, balance: new(big.Rat)}
		e.sessions[manifest] = current
	} else if current.runner != runner || current.payer != payer || current.price != priceInfo || current.unit != unit {
		return Challenge{}, errors.New("payment session scope conflict")
	}
	current.info, current.updated = wire.EncodeOrchestratorInfo(info), time.Now()
	return paymentChallenge(runner, manifest, service, current.info), nil
}

func paymentChallenge(runner, manifest, service string, info []byte) Challenge {
	return Challenge{PaymentParams: base64.StdEncoding.EncodeToString(info), Orchestrator: service, ManifestID: manifest, PaymentURL: service + "/apps/" + runner + "/session/" + manifest + "/payment"}
}

func (e *Engine) ChallengeInfo(manifest string) (wire.OrchestratorInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	current := e.sessions[manifest]
	if current == nil {
		return wire.OrchestratorInfo{}, ErrMissingChallenge
	}
	return wire.DecodeOrchestratorInfo(current.info)
}

func (e *Engine) ChallengeForManifest(runner, manifest, service string) (Challenge, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	current := e.sessions[manifest]
	if current == nil || current.runner != runner {
		return Challenge{}, ErrMissingChallenge
	}
	return paymentChallenge(runner, manifest, service, current.info), nil
}

func (e *Engine) RefreshChallenge(ctx context.Context, manifest string, payer ethcommon.Address, service string) (Challenge, error) {
	e.mu.Lock()
	current := e.sessions[manifest]
	if current == nil {
		e.mu.Unlock()
		return Challenge{}, ErrMissingChallenge
	}
	runner, expectedPayer, price, unit := current.runner, current.payer, current.price, current.unit
	e.mu.Unlock()
	if payer != expectedPayer {
		return Challenge{}, ErrInvalidPayment
	}
	return e.MakeChallenge(ctx, runner, manifest, payer, price.PricePerUnit, unit, service)
}

func ManifestFromSegment(header string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		return "", ErrInvalidPayment
	}
	segment, err := wire.DecodeSegData(data)
	if err != nil || segment.Auth.SessionID == "" || string(segment.ManifestID) != segment.Auth.SessionID {
		return "", ErrInvalidPayment
	}
	return segment.Auth.SessionID, nil
}

var ErrInvalidPayment = errors.New("invalid payment")
var ErrInsufficientBalance = errors.New("insufficient payment balance")
var ErrMissingChallenge = errors.New("payment challenge not found")
var ErrFixedAlreadyCharged = errors.New("fixed-price session already charged")

func decodeHeaders(paymentHeader, segmentHeader string) (wire.Payment, wire.SegData, error) {
	paymentBytes, err := base64.StdEncoding.DecodeString(paymentHeader)
	if err != nil {
		return wire.Payment{}, wire.SegData{}, ErrInvalidPayment
	}
	segmentBytes, err := base64.StdEncoding.DecodeString(segmentHeader)
	if err != nil {
		return wire.Payment{}, wire.SegData{}, ErrInvalidPayment
	}
	payment, err := wire.DecodePayment(paymentBytes)
	if err != nil {
		return wire.Payment{}, wire.SegData{}, ErrInvalidPayment
	}
	segment, err := wire.DecodeSegData(segmentBytes)
	if err != nil {
		return wire.Payment{}, wire.SegData{}, ErrInvalidPayment
	}
	return payment, segment, nil
}

func (e *Engine) Receive(ctx context.Context, runner, manifest, paymentHeader, segmentHeader string) (ethcommon.Address, *big.Rat, error) {
	payment, segment, err := decodeHeaders(paymentHeader, segmentHeader)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	if len(payment.PayerAddress) != 20 || len(segment.Hash) != 32 || string(segment.ManifestID) != manifest || segment.Auth.SessionID != manifest || len(segment.Signature) != 65 {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	payer := ethcommon.BytesToAddress(payment.PayerAddress)
	e.mu.Lock()
	session := e.sessions[manifest]
	if session == nil {
		e.mu.Unlock()
		return ethcommon.Address{}, nil, ErrMissingChallenge
	}
	runnerMatches, payerMatches, price := session.runner == runner, session.payer == payer, session.price
	e.mu.Unlock()
	params, recipientRand, err := e.authenticatePayment(payment, segment.Auth)
	if err != nil || !runnerMatches || !payerMatches || payment.ExpectedPrice != price || len(payment.PayerParams) == 0 || len(payment.PayerParams) > 100 {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	flatten := append([]byte(manifest), make([]byte, 32)...)
	flatten = append(flatten, segment.Hash...)
	if !(DefaultSigVerifier{}).Verify(payer, flatten, segment.Signature) {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	funds, err := e.chain.PayerFunds(ctx, payer, e.recipient)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	snapshot := funds.Snapshot
	active, err := e.chain.IsActiveAt(ctx, e.recipient, snapshot)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	if !active {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}

	if snapshot.Block == nil || snapshot.Round == nil || params.ExpirationParams.CreationRound < snapshot.Round.Int64()-2 || params.ExpirationParams.CreationRound > snapshot.Round.Int64() {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	if err := ValidatePayerFunds(funds); err != nil {
		return ethcommon.Address{}, nil, err
	}
	if funds.Reserve.Cmp(params.FaceValue) < 0 || funds.Deposit.Cmp(params.FaceValue) < 0 {
		return ethcommon.Address{}, nil, ErrPayerUnavailable
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Store winning tickets before publishing in-memory replay guards and credit.
	// The lock serializes receipt and accounting; failed batches change neither.
	if e.sessions[manifest] != session {
		return ethcommon.Address{}, nil, ErrMissingChallenge
	}
	e.observeBlock(snapshot.Block)
	if params.ExpirationBlock.Cmp(e.lastSeenBlock) <= 0 {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	if e.nonceCount+len(payment.PayerParams) > 1000000 {
		return ethcommon.Address{}, nil, errors.New("payment ticket capacity reached")
	}
	randKey := recipientRand.String()
	previous := e.ticketNonces[randKey]
	batchNonces := make(map[uint32]bool, len(payment.PayerParams))
	tx, err := e.store.db.BeginTx(ctx, nil)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	defer tx.Rollback()
	// Once redemption can reveal randomness, this epoch must never accept
	// more tickets, even if a reorg moves the observed L1 clock backwards.
	var exposed int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM winning_tickets w JOIN redemption_attempts a ON a.sig=w.sig WHERE w.recipient_rand_hash=? AND a.phase!='expired')`, params.RecipientRandHash.Hex()).Scan(&exposed); err != nil {
		return ethcommon.Address{}, nil, err
	}
	if exposed != 0 {
		return ethcommon.Address{}, nil, ErrInvalidPayment
	}
	balance := new(big.Rat).Set(session.balance)
	validator := NewValidator(DefaultSigVerifier{})
	for _, sp := range payment.PayerParams {
		if sp.TicketNonce == 0 || sp.TicketNonce >= 600 || len(sp.Sig) != 65 {
			return ethcommon.Address{}, nil, ErrInvalidPayment
		}
		ticket := NewTicket(&params, params.ExpirationParams, payer, sp.TicketNonce)
		if err := validator.ValidateTicket(e.recipient, ticket, sp.Sig, recipientRand); err != nil {
			return ethcommon.Address{}, nil, ErrInvalidPayment
		}
		if batchNonces[sp.TicketNonce] || (previous != nil && previous.nonceSeen[sp.TicketNonce]) {
			return ethcommon.Address{}, nil, ErrInvalidPayment
		}
		batchNonces[sp.TicketNonce] = true
		balance.Add(balance, ticket.EV())
		if validator.IsWinningTicket(ticket, sp.Sig, recipientRand) {
			_, err := tx.Exec(`INSERT INTO winning_tickets(payer_address,recipient,face_value,win_prob,ticket_nonce,recipient_rand,recipient_rand_hash,sig,creation_round,creation_round_block_hash,params_expiration_block)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`, payer.Hex(), ticket.Recipient.Hex(), ticket.FaceValue.Bytes(), ticket.WinProb.Bytes(), ticket.TicketNonce, ethcommon.LeftPadBytes(recipientRand.Bytes(), uint256Size), ticket.RecipientRandHash.Hex(), sp.Sig, ticket.CreationRound, ticket.CreationRoundBlockHash.Hex(), ticket.ParamsExpirationBlock.String())
			if err != nil {
				return ethcommon.Address{}, nil, err
			}
		}
	}
	// Include every unconfirmed winner, across all sessions for this payer.
	// Keep uncertain transactions reserved until finalized receipt reconciliation.
	rows, err := tx.Query("SELECT face_value FROM winning_tickets WHERE payer_address=? AND redeemed_at IS NULL AND creation_round>=? AND sig NOT IN (SELECT sig FROM redemption_attempts WHERE phase='reverted')", payer.Hex(), snapshot.Round.Int64()-2)
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	pending := new(big.Int)
	for rows.Next() {
		var face []byte
		if err := rows.Scan(&face); err != nil {
			rows.Close()
			return ethcommon.Address{}, nil, err
		}
		pending.Add(pending, new(big.Int).SetBytes(face))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return ethcommon.Address{}, nil, err
	}
	if pending.Cmp(new(big.Int).Add(funds.Deposit, funds.Reserve)) > 0 {
		return ethcommon.Address{}, nil, ErrPayerUnavailable
	}
	if err := tx.Commit(); err != nil {
		return ethcommon.Address{}, nil, err
	}
	if previous == nil {
		previous = &recipientNonces{nonceSeen: make(map[uint32]bool), expirationBlock: new(big.Int).Set(params.ExpirationBlock)}
		e.ticketNonces[randKey] = previous
	}
	for nonce := range batchNonces {
		previous.nonceSeen[nonce] = true
	}
	e.nonceCount += len(batchNonces)
	session.balance, session.updated = balance, time.Now()
	return payer, new(big.Rat).Set(balance), nil
}

func (e *Engine) Charge(ctx context.Context, manifest string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	current := e.sessions[manifest]
	if current == nil {
		return ErrMissingChallenge
	}
	units := int64(0)
	if current.unit == "fixed" {
		if !current.lastCharge.IsZero() {
			current.updated = time.Now()
			return ErrFixedAlreadyCharged
		}
		units = 1
	} else if current.unit == "seconds" && !current.lastCharge.IsZero() {
		units = max(int64(now.Sub(current.lastCharge)/time.Second), 0)
		if units > 3600 {
			return ErrInsufficientBalance
		}
		if units == 0 {
			return nil
		}
		now = current.lastCharge.Add(time.Duration(units) * time.Second)
	}
	fee := new(big.Rat).Mul(big.NewRat(current.price.PricePerUnit, current.price.UnitsPerPrice), big.NewRat(units, 1))
	if current.balance.Cmp(fee) < 0 {
		return ErrInsufficientBalance
	}
	current.balance.Sub(current.balance, fee)
	current.lastCharge, current.updated = now, time.Now()
	return nil
}

func (e *Engine) Balance(manifest string) (*big.Rat, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	current := e.sessions[manifest]
	if current == nil {
		return nil, ErrMissingChallenge
	}
	return new(big.Rat).Set(current.balance), nil
}

// ChallengePrice returns the agreed quote for a pending reservation, scoped to
// its runner. Updating the runner/feed must not change a previously issued quote.
func (e *Engine) ChallengePrice(runner, manifest string) (int64, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	current := e.sessions[manifest]
	if current == nil || current.runner != runner {
		return 0, "", ErrMissingChallenge
	}
	return current.price.PricePerUnit, current.unit, nil
}

// authenticatePayment reconstructs the PM commitment from the supplied fields,
// using go-livepeer's recipient HMAC.
func (e *Engine) authenticatePayment(payment wire.Payment, auth wire.AuthToken) (TicketParams, *big.Int, error) {
	p := payment.TicketParams
	if len(payment.PayerAddress) != 20 || len(p.Recipient) != 20 || ethcommon.BytesToAddress(p.Recipient) != e.recipient ||
		len(p.FaceValue) == 0 || len(p.FaceValue) > 32 || len(p.WinProb) == 0 || len(p.WinProb) > 32 ||
		len(p.Seed) == 0 || len(p.Seed) > 32 || len(p.ExpirationBlock) == 0 || len(p.ExpirationBlock) > 32 || len(p.RecipientRandHash) != 32 ||
		payment.Expiration.CreationRound <= 0 || len(payment.Expiration.CreationRoundBlockHash) != 32 ||
		payment.Expiration.CreationRound != p.Expiration.CreationRound || !bytes.Equal(payment.Expiration.CreationRoundBlockHash, p.Expiration.CreationRoundBlockHash) ||
		payment.ExpectedPrice.PricePerUnit <= 0 || payment.ExpectedPrice.UnitsPerPrice <= 0 || auth.SessionID == "" || time.Now().Unix() > auth.Expiration ||
		!hmac.Equal(auth.Token, e.authToken(auth.SessionID, auth.Expiration).Token) {
		return TicketParams{}, nil, ErrInvalidPayment
	}
	params := TicketParams{Recipient: e.recipient, FaceValue: new(big.Int).SetBytes(p.FaceValue), WinProb: new(big.Int).SetBytes(p.WinProb), RecipientRandHash: ethcommon.BytesToHash(p.RecipientRandHash), Seed: new(big.Int).SetBytes(p.Seed), ExpirationBlock: new(big.Int).SetBytes(p.ExpirationBlock), ExpirationParams: &TicketExpirationParams{CreationRound: payment.Expiration.CreationRound, CreationRoundBlockHash: ethcommon.BytesToHash(payment.Expiration.CreationRoundBlockHash)}}
	if params.FaceValue.Sign() <= 0 || params.WinProb.Sign() <= 0 || params.WinProb.Cmp(maxWinProb) >= 0 {
		return TicketParams{}, nil, ErrInvalidPayment
	}
	recipientRand := e.recipientRand(params.Seed, ethcommon.BytesToAddress(payment.PayerAddress), params.FaceValue, params.WinProb, params.ExpirationBlock, big.NewRat(payment.ExpectedPrice.PricePerUnit, payment.ExpectedPrice.UnitsPerPrice), params.ExpirationParams)
	if crypto.Keccak256Hash(ethcommon.LeftPadBytes(recipientRand.Bytes(), uint256Size)) != params.RecipientRandHash {
		return TicketParams{}, nil, ErrInvalidPayment
	}
	return params, recipientRand, nil
}
