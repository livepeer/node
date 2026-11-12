package orchestrator

import (
	"context"
	"math/big"
	"net/http"
	"testing"

	"github.com/livepeer/node/pm"
	"github.com/stretchr/testify/require"
)

func TestRunnerRetirementWaitsForDurableHandoffEvenAfterRegistrationLoss(t *testing.T) {
	_, store, ticket, _, _ := newRecoveryTest(t, 19)
	engine, err := NewPaymentEngine(store, paymentTestChain{}, ticket.Recipient, big.NewInt(1), big.NewInt(1))
	require.NoError(t, err)
	registry, server := reviewServer(t, "http://127.0.0.1:1")
	defer server.Close()
	server.SetPayment(engine)
	owner := appHeartbeat(t, registry, "owner", "bootstrap", registeredApp("app"))
	lifetime := registry.apps["app"].lifetime
	status, _ := registry.Unregister("owner", "wrong")
	require.Equal(t, http.StatusUnauthorized, status)
	require.False(t, lifetime.closed.Load())
	status, _ = registry.Unregister("owner", owner.HeartbeatSecret)
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.True(t, lifetime.closed.Load())
	// A retry, expiration or Node restart can leave no in-memory runner.
	status, _ = registry.Unregister("missing-owner", "bootstrap")
	require.Equal(t, http.StatusServiceUnavailable, status)
	_, err = store.db.Exec(`UPDATE ticket_state SET delivery='acknowledged' WHERE hash=?`, ticket.Hash().Bytes())
	require.NoError(t, err)
	status, _ = registry.Unregister("missing-owner", "bootstrap")
	require.Equal(t, http.StatusNotFound, status)
	var settled int
	require.NoError(t, store.db.QueryRow(`SELECT settled FROM ticket_state`).Scan(&settled))
	require.Zero(t, settled, "runner retirement does not wait for chain confirmation")
}

func TestRetiredAppRejectsPaymentReceiptStillWaitingOnRPC(t *testing.T) {
	engine, chain, key, _ := newRecipientTestEngine(t)
	registry, server := reviewServer(t, "http://127.0.0.1:1")
	defer server.Close()
	server.SetPayment(engine)
	owner := appHeartbeat(t, registry, "owner", "bootstrap", registeredApp("runner"))
	info := issueRecipientParams(t, engine, key)
	entered, release := make(chan struct{}), make(chan struct{})
	chain.funds = func(ctx context.Context, funds pm.PayerFunds) (pm.PayerFunds, error) {
		close(entered)
		select {
		case <-release:
			return funds, nil
		case <-ctx.Done():
			return funds, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { done <- receiveRecipientPayment(t, engine, key, info, 1) }()
	<-entered
	status, err := registry.Unregister("owner", owner.HeartbeatSecret)
	close(release)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.ErrorIs(t, <-done, ErrMissingChallenge)
	var count int
	require.NoError(t, engine.store.db.QueryRow(`SELECT count(*) FROM winning_tickets`).Scan(&count))
	require.Zero(t, count, "no receipt can commit after the normal drain acknowledgement")
}
