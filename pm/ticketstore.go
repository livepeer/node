package pm

import (
	ethcommon "github.com/ethereum/go-ethereum/common"
	"math/big"
)

// TicketStore is an interface which describes an object capable
// of persisting tickets
type TicketStore interface {
	// SelectEarliestWinningTicket selects the earliest stored winning ticket for a 'sender'
	// which is not yet redeemed
	SelectEarliestWinningTicket(sender ethcommon.Address, minCreationRound int64) (*SignedTicket, error)

	// RemoveWinningTicket removes a ticket
	RemoveWinningTicket(ticket *SignedTicket) error

	// StoreWinningTicket stores a signed ticket
	StoreWinningTicket(ticket *SignedTicket) error

	// MarkWinningTicketSubmitted stores a broadcast hash. A later receipt
	// confirmation sets redeemed_at; this call does not claim finality.
	MarkWinningTicketSubmitted(ticket *SignedTicket, txHash ethcommon.Hash) error

	// WinningTicketCount returns the amount of non-redeemed winning tickets for a sender in the TicketStore
	WinningTicketCount(sender ethcommon.Address, minCreationRound int64) (int, error)

	// IsOrchActive returns true if the given orchestrator addr is active in the given round
	IsOrchActive(addr ethcommon.Address, round *big.Int) (bool, error)
}
