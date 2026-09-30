package signer

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/livepeer/node/pm/wire"
)

// pricePolicy holds only a local observation of the exchange rate. Signer
// replicas use the same configured limits and feed, without sharing state.
type pricePolicy struct {
	mu                      sync.RWMutex
	maxLiveUSD, maxFixedUSD *big.Rat
	weiPerUSD               *big.Rat
	validUntil              time.Time
}

func newPricePolicy(liveUSD, fixedUSD string) (*pricePolicy, error) {
	live, ok := new(big.Rat).SetString(strings.TrimSpace(liveUSD))
	if !ok || live.Sign() <= 0 {
		return nil, errors.New("max-live-price-usd-per-second must be positive")
	}
	fixed, ok := new(big.Rat).SetString(strings.TrimSpace(fixedUSD))
	if !ok || fixed.Sign() <= 0 {
		return nil, errors.New("max-fixed-price-usd must be positive")
	}
	return &pricePolicy{maxLiveUSD: live, maxFixedUSD: fixed}, nil
}

func (p *pricePolicy) setRate(rate *big.Rat, until time.Time) error {
	if rate == nil || rate.Sign() <= 0 || (!until.IsZero() && !time.Now().Before(until)) {
		return errors.New("positive current wei-per-usd rate is required")
	}
	p.mu.Lock()
	p.weiPerUSD = new(big.Rat).Set(rate)
	p.validUntil = until
	p.mu.Unlock()
	return nil
}

func (p *pricePolicy) rate() (*big.Rat, bool) {
	if p == nil {
		return nil, false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.weiPerUSD == nil || (!p.validUntil.IsZero() && !time.Now().Before(p.validUntil)) {
		return nil, false
	}
	return new(big.Rat).Set(p.weiPerUSD), true
}

func (p *pricePolicy) ready() bool {
	_, ok := p.rate()
	return ok
}

func (p *pricePolicy) check(paymentType string, price wire.PriceInfo) error {
	rate, ok := p.rate()
	if !ok {
		return paymentFailure{503, "signer USD conversion rate unavailable"}
	}
	ceiling := p.maxLiveUSD
	if paymentType == "fixed" {
		ceiling = p.maxFixedUSD
	}
	actual := new(big.Rat).SetFrac64(price.PricePerUnit, price.UnitsPerPrice)
	if actual.Cmp(new(big.Rat).Mul(ceiling, rate)) > 0 {
		return paymentFailure{481, "orchestrator price exceeds signer USD maximum"}
	}
	return nil
}

func refreshPriceFeed(ctx context.Context, period time.Duration, policy *pricePolicy, read func(context.Context) (*big.Rat, time.Time, error)) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			rate, until, err := read(readCtx)
			cancel()
			if err == nil {
				err = policy.setRate(rate, until)
			}
			if err != nil {
				slog.Error("signer price feed unavailable", "error", err)
			}
		}
	}
}
