package signer

import (
	"context"
	"errors"
	"math/big"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livepeer/node/pm/wire"
	"github.com/stretchr/testify/require"
)

func TestPricePolicyExactCeilingsAndRateExpiry(t *testing.T) {
	hourly := big.NewRat(1200, 1) // $1200/hour = $1/3 per second.
	p, err := newPricePolicy(hourly, testRat(t, "1/2"))
	require.NoError(t, err)
	require.Equal(t, big.NewRat(1200, 1), hourly, "must not mutate configuration")
	require.False(t, p.ready())
	require.Equal(t, 503, p.check("live", wire.PriceInfo{PricePerUnit: 1, UnitsPerPrice: 1}).(paymentFailure).status)
	require.NoError(t, p.setRate(big.NewRat(3, 1), time.Now().Add(time.Minute)))
	require.True(t, p.ready())
	require.NoError(t, p.check("live", wire.PriceInfo{PricePerUnit: 1, UnitsPerPrice: 1}))
	require.Equal(t, 481, p.check("live", wire.PriceInfo{PricePerUnit: 4, UnitsPerPrice: 3}).(paymentFailure).status)
	require.NoError(t, p.check("fixed", wire.PriceInfo{PricePerUnit: 3, UnitsPerPrice: 2}))
	require.Equal(t, 481, p.check("fixed", wire.PriceInfo{PricePerUnit: 5, UnitsPerPrice: 3}).(paymentFailure).status)
	require.NoError(t, p.setRate(big.NewRat(6, 1), time.Now().Add(time.Minute)))
	require.NoError(t, p.check("live", wire.PriceInfo{PricePerUnit: 2, UnitsPerPrice: 1}), "refreshed rate must take effect")
	p.mu.Lock()
	p.validUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	require.False(t, p.ready())
	require.Equal(t, 503, p.check("fixed", wire.PriceInfo{PricePerUnit: 1, UnitsPerPrice: 1}).(paymentFailure).status)
	require.Error(t, p.setRate(big.NewRat(6, 1), time.Now().Add(-time.Second)))
}

func TestSignerUnavailableFeedThenRefresh(t *testing.T) {
	s, info := testService(t)
	policy, err := newPricePolicy(new(big.Rat).Mul(testRat(t, "20"), big.NewRat(3600, 1)), testRat(t, "20"))
	require.NoError(t, err)
	s.pricePolicy = policy
	req := map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed"}
	require.Equal(t, 503, postPayment(t, s, req).Code)
	require.False(t, policy.ready())
	var available atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		refreshPriceFeed(ctx, 5*time.Millisecond, policy, func(context.Context) (*big.Rat, time.Time, error) {
			if !available.Load() {
				return nil, time.Time{}, errors.New("feed unavailable")
			}
			return big.NewRat(1, 1), time.Now().Add(time.Second), nil
		})
	}()
	require.Equal(t, 503, postPayment(t, s, req).Code)
	available.Store(true)
	require.Eventually(t, policy.ready, time.Second, time.Millisecond)
	require.Equal(t, 200, postPayment(t, s, req).Code)
	cancel()
	<-done
	require.NoError(t, policy.setRate(big.NewRat(1, 1), time.Now().Add(10*time.Millisecond)))
	require.Eventually(t, func() bool { return !policy.ready() }, time.Second, time.Millisecond)
	require.Equal(t, 503, postPayment(t, s, req).Code)
}

func testURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func testRat(t *testing.T, raw string) *big.Rat {
	t.Helper()
	value, ok := new(big.Rat).SetString(raw)
	require.True(t, ok)
	return value
}
