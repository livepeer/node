package signer

import (
	"context"
	"math/big"
	"path/filepath"
	"sync"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/pm"
	"github.com/stretchr/testify/require"
)

type collateralChain struct{ withdraw *big.Int }

func (c collateralChain) Snapshot(context.Context) (pm.ChainSnapshot, error) {
	return pm.ChainSnapshot{Block: big.NewInt(50), Round: big.NewInt(5), RoundHash: ethcommon.HexToHash("0x1234")}, nil
}
func (c collateralChain) IsActiveAt(context.Context, ethcommon.Address, pm.ChainSnapshot) (bool, error) {
	return true, nil
}
func (c collateralChain) PayerFunds(ctx context.Context, _, _ ethcommon.Address) (pm.PayerFunds, error) {
	snapshot, err := c.Snapshot(ctx)
	return pm.PayerFunds{Snapshot: snapshot, Deposit: big.NewInt(10), Reserve: big.NewInt(10), WithdrawRound: c.withdraw}, err
}

func TestRecipientReservesWinningLiabilityAcrossConcurrentSessions(t *testing.T) {
	key, _, _ := testSignerKey(t)
	store, err := pm.OpenSQLite(filepath.Join(t.TempDir(), "recipient.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	chain := collateralChain{new(big.Int)}
	engine, err := pm.NewEngine(store, chain, ethcommon.HexToAddress("0x1234"), big.NewInt(10), new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), big.NewInt(1)))
	require.NoError(t, err)
	payments := map[string]paymentDraft{}
	for _, manifest := range []string{"one", "two", "three"} {
		_, err := engine.MakeChallenge(t.Context(), "runner", manifest, key.Address(), 9, "fixed", "https://orch.example")
		require.NoError(t, err)
		info, err := engine.ChallengeInfo(manifest)
		require.NoError(t, err)
		payment, err := (remotePayer{Signer: key, Chain: chain, Policy: pm.DefaultPayerPolicy()}).Generate(t.Context(), "fixed", manifest, info, paymentState{}, -1)
		require.NoError(t, err)
		payments[manifest] = payment
	}
	var workers sync.WaitGroup
	errors := make(chan error, 3)
	for manifest, payment := range payments {
		workers.Go(func() {
			_, _, err := engine.Receive(t.Context(), "runner", manifest, payment.Payment, payment.SegCreds)
			errors <- err
		})
	}
	workers.Wait()
	close(errors)
	accepted, rejected := 0, 0
	for err := range errors {
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, pm.ErrPayerUnavailable)
			rejected++
		}
	}
	require.Equal(t, 2, accepted)
	require.Equal(t, 1, rejected)
	count, err := store.WinningTicketCount(key.Address(), 0)
	require.NoError(t, err)
	require.Equal(t, 2, count, "rejected batches must roll back tickets and credit")
}
