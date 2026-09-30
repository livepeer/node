package pm

import (
	"math/big"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

type batchTestSigner struct{}

func (batchTestSigner) Address() ethcommon.Address         { return ethcommon.HexToAddress("0x1234") }
func (batchTestSigner) SignMessage([]byte) ([]byte, error) { return []byte{1, 2, 3}, nil }

func TestMakeRemoteBatchCarriesFractionalExpectedValue(t *testing.T) {
	params := TicketParams{Recipient: ethcommon.HexToAddress("0x5678"), FaceValue: big.NewInt(3), WinProb: new(big.Int).Div(new(big.Int).Set(maxWinProb), big.NewInt(2)), ExpirationBlock: big.NewInt(100), ExpirationParams: &TicketExpirationParams{CreationRound: 10}}
	batch, remaining, err := MakeRemoteBatch(params, batchTestSigner{}, 0, big.NewRat(2, 1), big.NewRat(0, 1))
	require.NoError(t, err)
	require.Len(t, batch.SenderParams, 2)
	require.Equal(t, uint32(1), batch.SenderParams[0].SenderNonce)
	require.Equal(t, uint32(2), batch.SenderParams[1].SenderNonce)
	require.True(t, remaining.Sign() > 0)
	require.True(t, remaining.Cmp(big.NewRat(1, 1)) < 0)
}

func TestMakeRemoteBatchRejectsExcessiveMint(t *testing.T) {
	params := TicketParams{FaceValue: big.NewInt(1), WinProb: new(big.Int).Sub(maxWinProb, big.NewInt(1)), ExpirationBlock: big.NewInt(100), ExpirationParams: &TicketExpirationParams{}}
	_, _, err := MakeRemoteBatch(params, batchTestSigner{}, 0, big.NewRat(101, 1), big.NewRat(0, 1))
	require.ErrorContains(t, err, "exceeds 100")
}
