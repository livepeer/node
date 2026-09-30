package pm

import (
	"context"
	"fmt"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
)

// RedeemPending submits each unattempted winning ticket once. SQLite records
// the attempt before broadcasting so an uncertain RPC outcome cannot silently
// trigger a second transaction. An operator can inspect recorded failures.
func RedeemPending(ctx context.Context, store *SQLiteStore, chain eth.PaymentChain, key *eth.Key, chainID, currentBlock *big.Int) []error {
	if store == nil || key == nil || chainID == nil || currentBlock == nil {
		return []error{fmt.Errorf("redemption is not configured")}
	}
	senders, err := store.PendingSenders()
	if err != nil {
		return []error{err}
	}
	var failures []error
	for _, sender := range senders {
		ticket, err := store.SelectEarliestWinningTicket(sender, 0)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if ticket == nil {
			continue
		}
		if ticket.ParamsExpirationBlock.Cmp(currentBlock) > 0 {
			continue
		}
		if err := store.ClaimRedemption(ticket); err != nil {
			failures = append(failures, err)
			continue
		}
		redeemTicket := eth.RedeemTicket{Recipient: ticket.Recipient, Sender: ticket.Sender, FaceValue: ticket.FaceValue, WinProb: ticket.WinProb, SenderNonce: ticket.SenderNonce, RecipientRandHash: ticket.RecipientRandHash, AuxData: ticket.AuxData(), Signature: ticket.Sig, RecipientRand: ticket.RecipientRand}
		hash, err := chain.Redeem(ctx, key, chainID, redeemTicket)
		if err != nil {
			_ = store.RecordRedemptionError(ticket, err)
			failures = append(failures, fmt.Errorf("sender %s: %w", sender.Hex(), err))
			continue
		}
		if err := store.MarkWinningTicketSubmitted(ticket, hash); err != nil {
			failures = append(failures, fmt.Errorf("transaction %s: %w", hash.Hex(), err))
		}
	}
	return failures
}

type ReceiptReader interface {
	Receipt(context.Context, ethcommon.Hash) (confirmed, reverted bool, err error)
}

// ReconcileSubmitted checks receipts once per background tick. It never
// broadcasts, so a pending or uncertain transaction cannot be sent twice.
func ReconcileSubmitted(ctx context.Context, store *SQLiteStore, chain ReceiptReader) []error {
	items, err := store.SubmittedRedemptions()
	if err != nil {
		return []error{err}
	}
	var failures []error
	for _, item := range items {
		confirmed, reverted, err := chain.Receipt(ctx, item.Hash)
		if err != nil {
			failures = append(failures, fmt.Errorf("receipt %s: %w", item.Hash.Hex(), err))
			continue
		}
		if confirmed {
			err = store.ConfirmRedemption(item.Signature)
		}
		if reverted {
			err = store.FailRedemption(item.Signature)
			failures = append(failures, fmt.Errorf("transaction %s reverted", item.Hash.Hex()))
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}
