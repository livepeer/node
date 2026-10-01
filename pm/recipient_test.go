package pm

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm/wire"
	"github.com/stretchr/testify/require"
)

// These vectors were produced by running recipient.rand, expiration AuxData,
// and orchestrator.AuthToken extracted from the pinned go-livepeer revision.
func TestRecipientHMACMatchesGoLivepeer(t *testing.T) {
	data, err := os.ReadFile("testdata/recipient-hmac.json")
	require.NoError(t, err)
	var fixture struct {
		Revision string `json:"source_revision"`
		Secret   string
		Vectors  []struct {
			Seed, Sender, FaceValue, WinProb, ExpirationBlock, Price, RoundHash, Session string
			Round, AuthExpiration                                                        int64
			Random, Commitment, Token                                                    string
		}
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	require.Equal(t, "bd645a09266833fb859053445d9ac85846330756", fixture.Revision)
	secret, err := hex.DecodeString(fixture.Secret)
	require.NoError(t, err)
	e := &Engine{}
	copy(e.secret[:], secret)
	copy(e.authSecret[:], secret)
	num := func(s string) *big.Int {
		n, ok := new(big.Int).SetString(s, 10)
		require.True(t, ok)
		return n
	}
	for _, v := range fixture.Vectors {
		t.Run(v.Session, func(t *testing.T) {
			price, ok := new(big.Rat).SetString(v.Price)
			require.True(t, ok)
			random := e.recipientRand(num(v.Seed), ethcommon.HexToAddress(v.Sender), num(v.FaceValue), num(v.WinProb), num(v.ExpirationBlock), price, &TicketExpirationParams{CreationRound: v.Round, CreationRoundBlockHash: ethcommon.HexToHash(v.RoundHash)})
			raw := ethcommon.LeftPadBytes(random.Bytes(), 32)
			require.Equal(t, v.Random, hex.EncodeToString(raw))
			require.Equal(t, v.Commitment, crypto.Keccak256Hash(raw).Hex())
			require.Equal(t, v.Token, hex.EncodeToString(e.authToken(v.Session, v.AuthExpiration).Token))
		})
	}
}

type recipientTestChain struct {
	block  int64
	active bool
}

func (c *recipientTestChain) Snapshot(context.Context) (ChainSnapshot, error) {
	return ChainSnapshot{Block: big.NewInt(c.block), Round: big.NewInt(5), RoundHash: ethcommon.HexToHash("0x1234")}, nil
}
func (c *recipientTestChain) IsActiveAt(context.Context, ethcommon.Address, ChainSnapshot) (bool, error) {
	return c.active, nil
}
func (c *recipientTestChain) SenderInfo(ctx context.Context, _, _ ethcommon.Address) (eth.SenderInfo, error) {
	snapshot, err := c.Snapshot(ctx)
	return eth.SenderInfo{Snapshot: snapshot, Deposit: big.NewInt(1000000000), Reserve: big.NewInt(1000000000), WithdrawRound: new(big.Int)}, err
}

func newRecipientTestEngine(t *testing.T) (*Engine, *recipientTestChain, *ecdsa.PrivateKey, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payments.sqlite")
	store, err := OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	chain := &recipientTestChain{active: true, block: 50}
	e, err := NewEngine(store, chain, ethcommon.HexToAddress("0x1234"), big.NewInt(10), new(big.Int).Sub(maxWinProb, big.NewInt(1)))
	require.NoError(t, err)
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	return e, chain, key, path
}

func issueRecipientParams(t *testing.T, e *Engine, key *ecdsa.PrivateKey) wire.OrchestratorInfo {
	t.Helper()
	challenge, err := e.MakeChallenge(t.Context(), "runner", "session", crypto.PubkeyToAddress(key.PublicKey), 9, "fixed", "https://orch.example")
	require.NoError(t, err)
	encoded, err := base64.StdEncoding.DecodeString(challenge.PaymentParams)
	require.NoError(t, err)
	info, err := wire.DecodeOrchestratorInfo(encoded)
	require.NoError(t, err)
	return info
}

func receiveRecipientPayment(t *testing.T, e *Engine, key *ecdsa.PrivateKey, info wire.OrchestratorInfo, nonces ...uint32) error {
	t.Helper()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	p := info.TicketParams
	params := TicketParams{Recipient: ethcommon.BytesToAddress(p.Recipient), FaceValue: new(big.Int).SetBytes(p.FaceValue), WinProb: new(big.Int).SetBytes(p.WinProb), RecipientRandHash: ethcommon.BytesToHash(p.RecipientRandHash), ExpirationBlock: new(big.Int).SetBytes(p.ExpirationBlock)}
	expiration := &TicketExpirationParams{CreationRound: p.Expiration.CreationRound, CreationRoundBlockHash: ethcommon.BytesToHash(p.Expiration.CreationRoundBlockHash)}
	sign := func(msg []byte) []byte {
		sig, err := crypto.Sign(accounts.TextHash(msg), key)
		require.NoError(t, err)
		sig[64] += 27
		return sig
	}
	payment := wire.Payment{Sender: sender.Bytes(), TicketParams: p, Expiration: p.Expiration, ExpectedPrice: info.Price}
	for _, nonce := range nonces {
		ticket := NewTicket(&params, expiration, sender, nonce)
		payment.SenderParams = append(payment.SenderParams, wire.TicketSenderParams{SenderNonce: nonce, Sig: sign(ticket.Hash().Bytes())})
	}
	hash := crypto.Keccak256(nil)
	flatten := append([]byte(info.Auth.SessionID), make([]byte, 32)...)
	flatten = append(flatten, hash...)
	segment := wire.SegData{ManifestID: []byte(info.Auth.SessionID), Hash: hash, Signature: sign(flatten), Auth: info.Auth}
	_, _, err := e.Receive(t.Context(), "runner", info.Auth.SessionID, base64.StdEncoding.EncodeToString(wire.EncodePayment(payment)), base64.StdEncoding.EncodeToString(wire.EncodeSegData(segment)))
	return err
}

func TestRecipientHMACRejectsTamperedParameters(t *testing.T) {
	e, _, key, _ := newRecipientTestEngine(t)
	original := issueRecipientParams(t, e, key)
	for name, mutate := range map[string]func(*wire.Payment, *wire.AuthToken){
		"sender":            func(p *wire.Payment, _ *wire.AuthToken) { p.Sender[0] ^= 1 },
		"recipient":         func(p *wire.Payment, _ *wire.AuthToken) { p.TicketParams.Recipient[0] ^= 1 },
		"seed":              func(p *wire.Payment, _ *wire.AuthToken) { p.TicketParams.Seed[0] ^= 1 },
		"face value":        func(p *wire.Payment, _ *wire.AuthToken) { p.TicketParams.FaceValue = big.NewInt(11).Bytes() },
		"win probability":   func(p *wire.Payment, _ *wire.AuthToken) { p.TicketParams.WinProb = big.NewInt(100).Bytes() },
		"expiry block":      func(p *wire.Payment, _ *wire.AuthToken) { p.TicketParams.ExpirationBlock = big.NewInt(91).Bytes() },
		"price numerator":   func(p *wire.Payment, _ *wire.AuthToken) { p.ExpectedPrice.PricePerUnit++ },
		"price denominator": func(p *wire.Payment, _ *wire.AuthToken) { p.ExpectedPrice.UnitsPerPrice++ },
		"round": func(p *wire.Payment, _ *wire.AuthToken) {
			p.Expiration.CreationRound++
			p.TicketParams.Expiration.CreationRound++
		},
		"round hash": func(p *wire.Payment, _ *wire.AuthToken) {
			p.Expiration.CreationRoundBlockHash = ethcommon.HexToHash("0x4321").Bytes()
			p.TicketParams.Expiration = p.Expiration
		},
		"commitment":   func(p *wire.Payment, _ *wire.AuthToken) { p.TicketParams.RecipientRandHash[0] ^= 1 },
		"auth token":   func(_ *wire.Payment, a *wire.AuthToken) { a.Token[0] ^= 1 },
		"auth expiry":  func(_ *wire.Payment, a *wire.AuthToken) { a.Expiration++ },
		"auth session": func(_ *wire.Payment, a *wire.AuthToken) { a.SessionID += "-forged" },
	} {
		t.Run(name, func(t *testing.T) {
			info, err := wire.DecodeOrchestratorInfo(wire.EncodeOrchestratorInfo(original))
			require.NoError(t, err)
			p := wire.Payment{Sender: crypto.PubkeyToAddress(key.PublicKey).Bytes(), TicketParams: info.TicketParams, Expiration: info.TicketParams.Expiration, ExpectedPrice: info.Price}
			_, _, err = e.authenticatePayment(p, info.Auth)
			require.NoError(t, err)
			mutate(&p, &info.Auth)
			_, _, err = e.authenticatePayment(p, info.Auth)
			require.ErrorIs(t, err, ErrInvalidPayment)
		})
	}
}

func TestRecipientBatchReplayAndExpiry(t *testing.T) {
	e, chain, key, _ := newRecipientTestEngine(t)
	info := issueRecipientParams(t, e, key)
	require.ErrorIs(t, receiveRecipientPayment(t, e, key, info, 1, 1), ErrInvalidPayment)
	balance, err := e.Balance("session")
	require.NoError(t, err)
	require.Zero(t, balance.Sign())
	count, err := e.store.WinningTicketCount(crypto.PubkeyToAddress(key.PublicKey), 0)
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, receiveRecipientPayment(t, e, key, info, 1), "a failed batch must not consume nonces")
	balance, err = e.Balance("session")
	require.NoError(t, err)
	balance.SetInt64(-1)
	before, err := e.Balance("session")
	require.NoError(t, err)
	require.Positive(t, before.Sign(), "callers must not mutate accounting state")
	_, err = e.RefreshChallenge(t.Context(), "session", crypto.PubkeyToAddress(key.PublicKey), "https://orch.example")
	require.NoError(t, err)
	// A newer valid auth token cannot reset nonce tracking for old PM params.
	info.Auth = e.authToken("session", time.Now().Add(2*time.Hour).Unix())
	require.ErrorIs(t, receiveRecipientPayment(t, e, key, info, 1), ErrInvalidPayment)
	after, err := e.Balance("session")
	require.NoError(t, err)
	require.Equal(t, before.RatString(), after.RatString(), "replay cannot add credit")
	require.NoError(t, receiveRecipientPayment(t, e, key, info, 2), "unexpired prior parameters still authenticate after refresh")
	chain.block = 90
	require.NoError(t, e.PruneControlState(t.Context()))
	require.Empty(t, e.senderNonces)
	require.Zero(t, e.nonceCount)
	chain.block = 89
	require.ErrorIs(t, receiveRecipientPayment(t, e, key, info, 1), ErrInvalidPayment, "a backward observation cannot revive pruned replay state")
	newInfo := issueRecipientParams(t, e, key)
	require.NoError(t, receiveRecipientPayment(t, e, key, newInfo, 1))
}

func TestRecipientRestartRotatesSecretsAndPreservesWinningTickets(t *testing.T) {
	e, chain, key, path := newRecipientTestEngine(t)
	info := issueRecipientParams(t, e, key)
	require.NoError(t, receiveRecipientPayment(t, e, key, info, 1))
	winner, err := e.store.SelectEarliestWinningTicket(crypto.PubkeyToAddress(key.PublicKey), 0)
	require.NoError(t, err)
	require.NotNil(t, winner)
	require.NoError(t, e.store.Close())
	store, err := OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	restarted, err := NewEngine(store, chain, e.recipient, e.faceValue, e.winProb)
	require.NoError(t, err)
	_, err = restarted.ChallengeInfo("session")
	require.ErrorIs(t, err, ErrMissingChallenge)
	fresh := issueRecipientParams(t, restarted, key)
	// Even a current auth token cannot authenticate the prior recipient's params.
	previousAuth := info.Auth
	info.Auth = fresh.Auth
	require.ErrorIs(t, receiveRecipientPayment(t, restarted, key, info, 1), ErrInvalidPayment)
	retained, err := store.SelectEarliestWinningTicket(crypto.PubkeyToAddress(key.PublicKey), 0)
	require.NoError(t, err)
	require.Equal(t, winner, retained)
	// Conversely, current PM params cannot reuse the previous auth token.
	fresh.Auth = previousAuth
	require.ErrorIs(t, receiveRecipientPayment(t, restarted, key, fresh, 1), ErrInvalidPayment)
	fresh.Auth = info.Auth
	require.NoError(t, receiveRecipientPayment(t, restarted, key, fresh, 1))
}

func TestInactiveOrchestratorDoesNotIssuePaidChallenge(t *testing.T) {
	e, chain, key, _ := newRecipientTestEngine(t)
	chain.active = false
	_, err := e.MakeChallenge(t.Context(), "runner", "session", crypto.PubkeyToAddress(key.PublicKey), 9, "fixed", "https://orch.example")
	require.ErrorContains(t, err, "not active")
}

func TestRecipientRejectsParametersOnceRedemptionPrepared(t *testing.T) {
	e, _, key, _ := newRecipientTestEngine(t)
	info := issueRecipientParams(t, e, key)
	require.NoError(t, receiveRecipientPayment(t, e, key, info, 1))
	before, err := e.Balance("session")
	require.NoError(t, err)
	ticket, err := e.store.SelectEarliestWinningTicket(crypto.PubkeyToAddress(key.PublicKey), 0)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	// A redemption may reveal the randomness even when this recipient's chain
	// observation still precedes parameter expiry.
	require.NoError(t, e.store.recordPrepared(t.Context(), ticket, eth.SignedTransaction{Hash: ethcommon.HexToHash("0x1234"), Raw: []byte{1}, From: e.recipient, Nonce: 1}))
	require.ErrorIs(t, receiveRecipientPayment(t, e, key, info, 2), ErrInvalidPayment)
	after, err := e.Balance("session")
	require.NoError(t, err)
	require.Equal(t, before.RatString(), after.RatString())
}
