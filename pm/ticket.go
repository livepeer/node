// Ticket types and encodings are adapted from livepeer/go-livepeer/pm/ticket.go
// at bd645a09266833fb859053445d9ac85846330756, by Yondon Fu, Elad Mallel,
// and Nico Vergauwen. The original source commits are retained in Git history.
package pm

import (
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Constants for byte sizes of Solidity types
const (
	addressSize = 20
	uint256Size = 32
	bytes32Size = 32
)

// maxWinProb is the largest uint256 and the denominator for ticket odds.
var maxWinProb = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// SignedTicket is a wrapper around a Ticket with the payer's signature over the ticket and
// the recipient recipientRand
type SignedTicket struct {
	// Ticket contains ticket fields that are directly
	// accessible on SignedTicket since it is embedded
	*Ticket

	// Sig is the payer's signature over the ticket
	Sig []byte

	// RecipientRand is the recipient's random value that should be
	// the preimage for the ticket's recipientRandHash
	RecipientRand *big.Int
}

// TicketParams represents the parameters defined by a receiver that a payer must adhere to when
// sending tickets to receiver.
type TicketParams struct {
	Recipient ethcommon.Address

	FaceValue *big.Int

	WinProb *big.Int

	RecipientRandHash ethcommon.Hash

	Seed *big.Int

	ExpirationBlock *big.Int

	ExpirationParams *TicketExpirationParams
}

// WinProbRat returns the ticket WinProb as a percentage represented as a big.Rat
func (p *TicketParams) WinProbRat() *big.Rat {
	return winProbRat(p.WinProb)
}

// TicketExpirationParams indicates when/how a ticket expires
type TicketExpirationParams struct {
	CreationRound int64

	CreationRoundBlockHash ethcommon.Hash
}

// TicketPayerParams contains a ticket nonce and the payer's signature over its hash.
type TicketPayerParams struct {
	TicketNonce uint32

	Sig []byte
}

// TicketBatch is a group of tickets that share the same TicketParams, TicketExpirationParams and PayerAddress
// Each ticket in a batch is identified by a unique TicketPayerParams
type TicketBatch struct {
	*TicketParams
	*TicketExpirationParams

	PayerAddress ethcommon.Address

	PayerParams []*TicketPayerParams
}

// Tickets returns the tickets in the batch
func (b *TicketBatch) Tickets() []*Ticket {
	var tickets []*Ticket
	for i := 0; i < len(b.PayerParams); i++ {
		ticket := &Ticket{
			Recipient:              b.Recipient,
			PayerAddress:           b.PayerAddress,
			FaceValue:              b.FaceValue,
			WinProb:                b.WinProb,
			TicketNonce:            b.PayerParams[i].TicketNonce,
			RecipientRandHash:      b.RecipientRandHash,
			CreationRound:          b.CreationRound,
			CreationRoundBlockHash: b.CreationRoundBlockHash,
		}
		tickets = append(tickets, ticket)
	}

	return tickets
}

// Ticket is lottery ticket payment in a probabilistic micropayment protocol
// The expected value of the ticket constitutes the payment and can be
// calculated using the ticket's face value and winning probability
type Ticket struct {
	// Recipient is the ETH address of recipient
	Recipient ethcommon.Address

	// PayerAddress identifies the account that signs the ticket and funds its payment.
	PayerAddress ethcommon.Address

	// FaceValue represents the pay out to
	// the recipient if the ticket wins
	FaceValue *big.Int

	// WinProb represents how likely a ticket will win
	WinProb *big.Int

	// TicketNonce is the monotonically increasing counter that makes
	// each ticket unique given a particular recipientRand value
	TicketNonce uint32

	// RecipientRandHash is the 32 byte keccak-256 hash commitment to a random number
	// provided by the recipient. In order for the recipient to redeem
	// a winning ticket, it must reveal the preimage to this hash
	RecipientRandHash ethcommon.Hash

	// CreationRound is the round during which the ticket is created
	CreationRound int64

	// CreationRoundBlockHash is the block hash associated with CreationRound
	CreationRoundBlockHash ethcommon.Hash

	// ParamsExpirationBlock is the block number at which the ticket parameters used
	// to create the ticket will no longer be valid
	ParamsExpirationBlock *big.Int
}

// NewTicket creates a Ticket instance
func NewTicket(params *TicketParams, expirationParams *TicketExpirationParams, payer ethcommon.Address, ticketNonce uint32) *Ticket {
	return &Ticket{
		Recipient:              params.Recipient,
		PayerAddress:           payer,
		FaceValue:              params.FaceValue,
		WinProb:                params.WinProb,
		TicketNonce:            ticketNonce,
		RecipientRandHash:      params.RecipientRandHash,
		CreationRound:          expirationParams.CreationRound,
		CreationRoundBlockHash: expirationParams.CreationRoundBlockHash,
		ParamsExpirationBlock:  params.ExpirationBlock,
	}
}

// EV returns the expected value of a ticket
func (t *Ticket) EV() *big.Rat {
	return ticketEV(t.FaceValue, t.WinProb)
}

// WinProbRat returns the ticket WinProb as a percentage represented as a big.Rat
func (t *Ticket) WinProbRat() *big.Rat {
	return winProbRat(t.WinProb)
}

// Hash returns the keccak-256 hash of the ticket's fields as tightly packed
// arguments as described in the Solidity documentation
// See: https://solidity.readthedocs.io/en/v0.4.25/units-and-global-variables.html#mathematical-and-cryptographic-functions
func (t *Ticket) Hash() ethcommon.Hash {
	return crypto.Keccak256Hash(t.flatten())
}

// AuxData returns the ticket's CreationRound and CreationRoundBlockHash encoded into a byte array:
// [0:31] = CreationRound (left padded with zero bytes)
// [32..63] = CreationRoundBlockHash
// See: https://github.com/livepeer/protocol/blob/pm/contracts/pm/mixins/MixinTicketProcessor.sol#L94
func (t *Ticket) AuxData() []byte {
	return (&TicketExpirationParams{
		CreationRound:          t.CreationRound,
		CreationRoundBlockHash: t.CreationRoundBlockHash,
	}).AuxData()
}

func (t *Ticket) flatten() []byte {
	auxData := t.AuxData()

	buf := make([]byte, addressSize+addressSize+uint256Size+uint256Size+uint256Size+bytes32Size+len(auxData))
	i := copy(buf[0:], t.Recipient.Bytes())
	i += copy(buf[i:], t.PayerAddress.Bytes())
	i += copy(buf[i:], ethcommon.LeftPadBytes(t.FaceValue.Bytes(), uint256Size))
	i += copy(buf[i:], ethcommon.LeftPadBytes(t.WinProb.Bytes(), uint256Size))
	i += copy(buf[i:], ethcommon.LeftPadBytes(new(big.Int).SetUint64(uint64(t.TicketNonce)).Bytes(), uint256Size))
	i += copy(buf[i:], t.RecipientRandHash.Bytes())

	if len(auxData) > 0 {
		copy(buf[i:], auxData)
	}

	return buf
}

func (e *TicketExpirationParams) AuxData() []byte {
	if e.CreationRound == 0 && (e.CreationRoundBlockHash == ethcommon.Hash{}) {
		// Return empty byte array if both values are 0
		return []byte{}
	}

	return append(
		ethcommon.LeftPadBytes(big.NewInt(e.CreationRound).Bytes(), uint256Size),
		e.CreationRoundBlockHash.Bytes()...,
	)
}

func ticketEV(faceValue *big.Int, winProb *big.Int) *big.Rat {
	return new(big.Rat).Mul(new(big.Rat).SetInt(faceValue), new(big.Rat).SetFrac(winProb, maxWinProb))
}

func winProbRat(winProb *big.Int) *big.Rat {
	return new(big.Rat).SetFrac(winProb, maxWinProb)
}
