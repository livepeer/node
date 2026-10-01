package pm

import (
	"math/big"
	"testing"

	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

// Ported economic boundaries from go-livepeer/pm/sender_test.go, by Yondon Fu,
// Nico Vergauwen, and Rafał Leszko; see the source credits in senderpolicy.go.
func TestSenderExposurePolicy(t *testing.T) {
	for _, scenario := range []string{"valid", "certain-win", "ticket-ev", "batch-ev", "face-cap", "unlock-current", "unlock-next", "no-reserve"} {
		t.Run(scenario, func(t *testing.T) {
			params := TicketParams{FaceValue: big.NewInt(100), WinProb: new(big.Int).Quo(maxWinProb, big.NewInt(10))}
			funds := eth.SenderInfo{Snapshot: eth.ChainSnapshot{Round: big.NewInt(5)}, Deposit: big.NewInt(1000), Reserve: big.NewInt(100), WithdrawRound: new(big.Int)}
			policy := SenderPolicy{MaxTicketEV: big.NewRat(10, 1), MaxBatchEV: big.NewRat(100, 1), DepositMultiplier: 10}
			count := 1
			switch scenario {
			case "certain-win":
				params.WinProb = new(big.Int).Set(maxWinProb)
			case "ticket-ev":
				policy.MaxTicketEV = big.NewRat(9, 1)
			case "batch-ev":
				count = 11
			case "face-cap":
				funds.Deposit = big.NewInt(999)
			case "unlock-current":
				funds.WithdrawRound = big.NewInt(5)
			case "unlock-next":
				funds.WithdrawRound = big.NewInt(6)
			case "no-reserve":
				funds.Reserve = new(big.Int)
			}
			err := policy.Check(params, count, funds)
			if scenario == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
