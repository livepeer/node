package pm

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

type inactivePaymentChain struct{}

func (inactivePaymentChain) Snapshot(context.Context) (ChainSnapshot, error) {
	return ChainSnapshot{Block: big.NewInt(50), Round: big.NewInt(5), RoundHash: ethcommon.HexToHash("0x1234")}, nil
}
func (inactivePaymentChain) IsActiveAt(context.Context, ethcommon.Address, ChainSnapshot) (bool, error) {
	return false, nil
}
func (c inactivePaymentChain) SenderInfo(ctx context.Context, _, _ ethcommon.Address) (eth.SenderInfo, error) {
	snapshot, err := c.Snapshot(ctx)
	return eth.SenderInfo{Snapshot: snapshot, Deposit: big.NewInt(1000000000), Reserve: big.NewInt(1000000000), WithdrawRound: new(big.Int)}, err
}

func TestInactiveOrchestratorDoesNotIssuePaidChallenge(t *testing.T) {
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "recipient.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	engine, err := NewEngine(store, inactivePaymentChain{}, ethcommon.HexToAddress("0x1234"), big.NewInt(10), big.NewInt(100))
	require.NoError(t, err)
	_, err = engine.MakeChallenge(t.Context(), "runner", "manifest", ethcommon.HexToAddress("0x5678"), 10, "fixed", "https://orchestrator.example")
	require.ErrorContains(t, err, "not active")
}
