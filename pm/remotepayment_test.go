package pm

import (
	"math/big"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestDraftRemoteBatchCreditAndNonceBoundaries(t *testing.T) {
	// maxWinProb is divisible by three, so each ticket has exactly 10/3 wei EV.
	params := TicketParams{Recipient: ethcommon.HexToAddress("0x5678"), FaceValue: big.NewInt(10), WinProb: new(big.Int).Quo(maxWinProb, big.NewInt(3)), ExpirationBlock: big.NewInt(100), ExpirationParams: &TicketExpirationParams{CreationRound: 10}}
	for _, tt := range []struct {
		name                    string
		fee, balance, remaining *big.Rat
		firstNonce              uint32
		count                   int
		want                    error
		message                 string
	}{
		{"fractional change", big.NewRat(5, 1), new(big.Rat), big.NewRat(5, 3), 0, 2, nil, ""},
		{"EV floor despite prepaid fee", big.NewRat(1, 1), big.NewRat(2, 1), big.NewRat(13, 3), 41, 1, nil, ""},
		{"at EV floor", big.NewRat(1, 1), big.NewRat(10, 3), nil, 0, 0, ErrNoTickets, ""},
		{"larger fee shortfall", big.NewRat(10, 1), big.NewRat(10, 3), new(big.Rat), 0, 2, nil, ""},
		{"fee prepaid", big.NewRat(10, 1), big.NewRat(10, 1), nil, 0, 0, ErrNoTickets, ""},
		{"one wei short", big.NewRat(10, 1), big.NewRat(9, 1), big.NewRat(7, 3), 0, 1, nil, ""},
		{"last nonce", big.NewRat(1, 1), new(big.Rat), big.NewRat(7, 3), 598, 1, nil, ""},
		{"nonce exhausted", big.NewRat(1, 1), new(big.Rat), nil, 599, 0, ErrRefreshRequired, ""},
		{"oversized batch", big.NewRat(1001, 3), new(big.Rat), nil, 0, 0, nil, "exceeds 100"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			balance := new(big.Rat).Set(tt.balance)
			batch, remaining, err := DraftRemoteBatch(params, ethcommon.HexToAddress("0x1234"), tt.firstNonce, tt.fee, balance)
			require.Equal(t, tt.balance.RatString(), balance.RatString(), "the caller owns its prior balance")
			if tt.message != "" {
				require.ErrorContains(t, err, tt.message)
				return
			}
			require.ErrorIs(t, err, tt.want)
			if tt.want != nil {
				return
			}
			require.Len(t, batch.PayerParams, tt.count)
			require.Equal(t, tt.remaining.RatString(), remaining.RatString())
			for i, ticket := range batch.PayerParams {
				require.Equal(t, tt.firstNonce+uint32(i)+1, ticket.TicketNonce)
			}
		})
	}
}
