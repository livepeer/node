package pm

import (
	"math/big"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

type stubSigVerifier bool

func (s stubSigVerifier) Verify(ethcommon.Address, []byte, []byte) bool { return bool(s) }

func TestValidateTicket(t *testing.T) {
	recipient := ethcommon.HexToAddress("73AEd7b5dEb30222fa896f399d46cC99c7BEe57F")
	payer := ethcommon.HexToAddress("A69cdA26600c155cF2c150964Bdb5371ac3f606F")
	random := big.NewInt(10)
	commitment := crypto.Keccak256Hash(ethcommon.LeftPadBytes(random.Bytes(), uint256Size))
	for _, tt := range []struct {
		name             string
		recipient, payer ethcommon.Address
		commitment       ethcommon.Hash
		validSignature   bool
		want             error
	}{
		{"valid", recipient, payer, commitment, true, nil},
		{"wrong recipient", payer, payer, commitment, true, errInvalidTicketRecipient},
		{"zero payer", recipient, ethcommon.Address{}, commitment, true, errInvalidTicketPayer},
		{"wrong preimage", recipient, payer, ethcommon.Hash{}, true, errInvalidTicketRecipientRand},
		{"invalid signature", recipient, payer, commitment, false, errInvalidTicketSignature},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := NewValidator(stubSigVerifier(tt.validSignature))
			ticket := &Ticket{Recipient: tt.recipient, PayerAddress: tt.payer, RecipientRandHash: tt.commitment, FaceValue: big.NewInt(10), WinProb: big.NewInt(100)}
			require.ErrorIs(t, v.ValidateTicket(recipient, ticket, []byte("signature"), random), tt.want)
		})
	}
}

func TestWinningTicketThreshold(t *testing.T) {
	sig, random := []byte("foo"), big.NewInt(10)
	outcome := new(big.Int).SetBytes(crypto.Keccak256(sig, ethcommon.LeftPadBytes(random.Bytes(), bytes32Size)))
	for _, tt := range []struct {
		name        string
		probability *big.Int
		wins        bool
	}{
		{"zero", new(big.Int), false},
		{"equal to outcome", outcome, false},
		{"just above outcome", new(big.Int).Add(outcome, big.NewInt(1)), true},
		{"maximum", maxWinProb, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wins, NewValidator(stubSigVerifier(true)).IsWinningTicket(&Ticket{WinProb: tt.probability}, sig, random))
		})
	}
}
