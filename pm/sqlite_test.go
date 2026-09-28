package pm

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

type receiptFixture struct{ confirmed, reverted bool }

func (r receiptFixture) Receipt(context.Context, ethcommon.Hash) (bool, bool, error) {
	return r.confirmed, r.reverted, nil
}

func TestPaymentSQLiteFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payment.sqlite")
	store, err := OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.NoError(t, os.Chmod(path, 0644))
	_, err = OpenSQLite(path)
	require.ErrorContains(t, err, "owner-only")
}

// Adapted from go-livepeer/common/db_test.go's winning-ticket store tests.
func TestSQLiteWinningTicketLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipient.sqlite")
	store, err := OpenSQLite(path)
	require.NoError(t, err)
	sender := ethcommon.HexToAddress("0x1234")
	ticket := &SignedTicket{Ticket: &Ticket{Sender: sender, Recipient: ethcommon.HexToAddress("0x5678"),
		FaceValue: big.NewInt(1234), WinProb: big.NewInt(2345), SenderNonce: 123,
		RecipientRandHash: ethcommon.HexToHash("0xabcd"), CreationRound: 9,
		CreationRoundBlockHash: ethcommon.HexToHash("0x9876"), ParamsExpirationBlock: big.NewInt(1337)},
		Sig: []byte{1, 2, 3}, RecipientRand: big.NewInt(4567)}
	count, err := store.WinningTicketCount(sender, 9)
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, store.StoreWinningTicket(ticket))
	require.Error(t, store.StoreWinningTicket(ticket))
	selected, err := store.SelectEarliestWinningTicket(sender, 9)
	require.NoError(t, err)
	require.Equal(t, ticket, selected)
	selected, err = store.SelectEarliestWinningTicket(sender, 10)
	require.NoError(t, err)
	require.Nil(t, selected)
	require.NoError(t, store.Close())
	store, err = OpenSQLite(path)
	require.NoError(t, err)
	defer store.Close()
	selected, err = store.SelectEarliestWinningTicket(sender, 9)
	require.NoError(t, err)
	require.Equal(t, ticket, selected)
	require.NoError(t, store.ClaimRedemption(ticket))
	require.NoError(t, store.MarkWinningTicketSubmitted(ticket, ethcommon.HexToHash("0x1234")))
	require.Error(t, store.MarkWinningTicketSubmitted(ticket, ethcommon.HexToHash("0x1234")))
	submitted, err := store.SubmittedRedemptions()
	require.NoError(t, err)
	require.Len(t, submitted, 1)
	require.Equal(t, ethcommon.HexToHash("0x1234"), submitted[0].Hash)
	require.Empty(t, ReconcileSubmitted(t.Context(), store, receiptFixture{confirmed: true}))
	submitted, err = store.SubmittedRedemptions()
	require.NoError(t, err)
	require.Empty(t, submitted)
	count, err = store.WinningTicketCount(sender, 9)
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, store.RemoveWinningTicket(ticket))
}

func TestRevertedRedemptionIsRecordedWithoutRetry(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "recipient.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	sender := ethcommon.HexToAddress("0x1234")
	ticket := &SignedTicket{Ticket: &Ticket{Sender: sender, Recipient: ethcommon.HexToAddress("0x5678"), FaceValue: big.NewInt(1), WinProb: big.NewInt(1), SenderNonce: 1, RecipientRandHash: ethcommon.HexToHash("0xabcd"), CreationRound: 1, ParamsExpirationBlock: big.NewInt(10)}, Sig: []byte{1, 2, 3}, RecipientRand: big.NewInt(4)}
	require.NoError(t, store.StoreWinningTicket(ticket))
	require.NoError(t, store.ClaimRedemption(ticket))
	require.NoError(t, store.MarkWinningTicketSubmitted(ticket, ethcommon.HexToHash("0x1234")))
	require.Len(t, ReconcileSubmitted(t.Context(), store, receiptFixture{reverted: true}), 1)
	submitted, err := store.SubmittedRedemptions()
	require.NoError(t, err)
	require.Empty(t, submitted)
	pending, err := store.PendingSenders()
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestSQLiteOrchestratorRoundView(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "recipient.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	addr := ethcommon.HexToAddress("0x1234")
	round := big.NewInt(99)
	active, err := store.IsOrchActive(addr, round)
	require.NoError(t, err)
	require.False(t, active)
	require.NoError(t, store.SetOrchestratorActive(addr, round, true))
	active, err = store.IsOrchActive(addr, round)
	require.NoError(t, err)
	require.True(t, active)
	require.NoError(t, store.SetOrchestratorActive(addr, round, false))
	active, err = store.IsOrchActive(addr, round)
	require.NoError(t, err)
	require.False(t, active)
}
