package pm

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

type inactivePaymentChain struct{}

func (inactivePaymentChain) Snapshot(context.Context) (ChainSnapshot, error) {
	return ChainSnapshot{Block: big.NewInt(50), Round: big.NewInt(5), RoundHash: ethcommon.HexToHash("0x1234")}, nil
}
func (inactivePaymentChain) IsActive(context.Context, ethcommon.Address) (bool, error) {
	return false, nil
}
func (inactivePaymentChain) ValidateSender(context.Context, ethcommon.Address, *big.Int) error {
	return nil
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
