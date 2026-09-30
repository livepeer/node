package orchestrator

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPendingPaymentDoesNotExposeOrBillSession(t *testing.T) {
	r := NewRegistry("", "https://orch.example", time.Second, time.Minute)
	r.SetWeiPerUSD(big.NewRat(10, 1))
	require.NoError(t, r.AddStatic(StaticRunner{ID: "r", RunnerURL: "https://runner.example", App: "app", Proxy: true, PriceInfo: priceInfo{Price: "1", Currency: "usd", Unit: "fixed"}}))
	quote, _, err := r.PriceForRunner("r")
	require.NoError(t, err)
	var events []string
	r.onEvent = func(_, event, _ string) { events = append(events, event) }
	id, _, _, _, err := r.reserveWithPrice("r", "payment-manifest", &quote)
	require.NoError(t, err)
	require.Empty(t, events)
	require.Empty(t, r.PaidSessions())
	_, _, status, err := r.sessionTarget("r", id)
	require.Error(t, err)
	require.Equal(t, 404, status)
	for proxy := range r.runners["r"].Sessions[id].Proxies {
		_, _, _, _, ok := r.proxyTarget(proxy)
		require.False(t, ok)
	}
	require.True(t, r.activateSession("r", id))
	require.Equal(t, []string{"reserved"}, events)
	require.Equal(t, []string{id}, r.PaidSessions())
	_, _, status, err = r.sessionTarget("r", id)
	require.NoError(t, err)
	require.Equal(t, 200, status)
	r.ReleaseBySession(id)
	require.False(t, r.activateSession("r", id), "a released pending reservation cannot be resurrected")
}
