package orchestrator

import (
	"math/big"
	"path/filepath"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/test"
	"github.com/livepeer/node/pm"
	"github.com/stretchr/testify/require"
)

func TestOldCreationRoundIsNotBroadcast(t *testing.T) {
	path, passwordPath := test.WriteFixedKeystore(t)
	key, err := eth.OpenKeystoreFile(path, passwordPath)
	require.NoError(t, err)
	store, err := OpenRedeemerDB(filepath.Join(t.TempDir(), "payment.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	payer := ethcommon.HexToAddress("0x1234")
	ticket := &pm.SignedTicket{Ticket: &pm.Ticket{PayerAddress: payer, Recipient: key.Address(), FaceValue: big.NewInt(10), WinProb: big.NewInt(100), TicketNonce: 1, RecipientRandHash: ethcommon.HexToHash("0xabcd"), CreationRound: 5, ParamsExpirationBlock: big.NewInt(10)}, Sig: []byte{1, 2, 3}, RecipientRand: big.NewInt(4)}
	require.NoError(t, store.StoreWinningTicket(ticket))
	failures := RedeemPending(t.Context(), store, eth.PaymentChain{}, key, big.NewInt(1), eth.ChainSnapshot{Block: big.NewInt(10), Round: big.NewInt(8)})
	require.Empty(t, failures)
	pending, err := store.PendingPayers()
	require.NoError(t, err)
	require.Empty(t, pending)
}
