package signer

import (
	"context"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
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
func (c collateralChain) SenderInfo(ctx context.Context, _, _ ethcommon.Address) (eth.SenderInfo, error) {
	snapshot, err := c.Snapshot(ctx)
	return eth.SenderInfo{Snapshot: snapshot, Deposit: big.NewInt(10), Reserve: big.NewInt(10), WithdrawRound: c.withdraw}, err
}

func TestRecipientReservesWinningLiabilityAcrossConcurrentSessions(t *testing.T) {
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(path, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	key, err := eth.OpenKeyFile(path)
	require.NoError(t, err)
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
		payment, err := (remoteSender{Signer: key, Chain: chain, Policy: pm.DefaultSenderPolicy()}).Generate(t.Context(), "fixed", manifest, info, paymentState{}, -1)
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
			require.ErrorIs(t, err, pm.ErrSenderUnavailable)
			rejected++
		}
	}
	require.Equal(t, 2, accepted)
	require.Equal(t, 1, rejected)
	count, err := store.WinningTicketCount(key.Address(), 0)
	require.NoError(t, err)
	require.Equal(t, 2, count, "rejected batches must roll back tickets and credit")
}

func TestRecipientRejectsExposedRandomnessAfterClockRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	key, err := eth.OpenKeyFile(path)
	require.NoError(t, err)
	store, err := pm.OpenSQLite(filepath.Join(t.TempDir(), "recipient.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	chain := collateralChain{new(big.Int)}
	engine, err := pm.NewEngine(store, chain, ethcommon.HexToAddress("0x1234"), big.NewInt(10), new(big.Int).Sub(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), big.NewInt(1)))
	require.NoError(t, err)
	_, err = engine.MakeChallenge(t.Context(), "runner", "session", key.Address(), 9, "fixed", "https://orch.example")
	require.NoError(t, err)
	info, err := engine.ChallengeInfo("session")
	require.NoError(t, err)
	sender := remoteSender{Signer: key, Chain: chain, Policy: pm.DefaultSenderPolicy()}
	first, err := sender.Generate(t.Context(), "fixed", "session", info, paymentState{}, -1)
	require.NoError(t, err)
	_, before, err := engine.Receive(t.Context(), "runner", "session", first.Payment, first.SegCreds)
	require.NoError(t, err)
	_, _, err = engine.Receive(t.Context(), "runner", "session", first.Payment, first.SegCreds)
	require.ErrorIs(t, err, pm.ErrInvalidPayment, "recipient must reject a duplicate ticket from a stateless signer retry")
	unchanged, err := engine.Balance("session")
	require.NoError(t, err)
	require.Equal(t, before.RatString(), unchanged.RatString())
	ticket, err := store.SelectEarliestWinningTicket(key.Address(), 0)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	// An existing uncertain attempt may have exposed the randomness. Simulate
	// a clock rollback with the chain fixture still before parameter expiry.
	require.NoError(t, store.ClaimRedemption(ticket))
	state := first.State
	second, err := sender.Generate(t.Context(), "fixed", "session", info, state, 0)
	require.NoError(t, err)
	_, _, err = engine.Receive(t.Context(), "runner", "session", second.Payment, second.SegCreds)
	require.ErrorIs(t, err, pm.ErrInvalidPayment)
	after, err := engine.Balance("session")
	require.NoError(t, err)
	require.Equal(t, before.RatString(), after.RatString())
}
