package pm

import (
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

func TestUnexpiredWinningTicketIsNotBroadcast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("0", 63)+"1"), 0600))
	key, err := eth.OpenKeyFile(path)
	require.NoError(t, err)
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "payment.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	sender := ethcommon.HexToAddress("0x1234")
	ticket := &SignedTicket{Ticket: &Ticket{Sender: sender, Recipient: key.Address(), FaceValue: big.NewInt(10), WinProb: big.NewInt(100), SenderNonce: 1, RecipientRandHash: ethcommon.HexToHash("0xabcd"), CreationRound: 5, ParamsExpirationBlock: big.NewInt(10)}, Sig: []byte{1, 2, 3}, RecipientRand: big.NewInt(4)}
	require.NoError(t, store.StoreWinningTicket(ticket))
	failures := RedeemPending(t.Context(), store, eth.PaymentChain{}, key, big.NewInt(1), big.NewInt(9))
	require.Empty(t, failures)
	pending, err := store.PendingSenders()
	require.NoError(t, err)
	require.Equal(t, []ethcommon.Address{sender}, pending)
}
