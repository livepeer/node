package pm

import (
	"errors"
	"math/big"
	"strings"
)

// ConvertRunnerPrice converts a runner's USD quote to whole wei per second or
// per fixed job. The exchange rate is supplied explicitly by the operator.
// Rounding upward avoids advertising a zero-wei charge for a positive quote.
func ConvertRunnerPrice(priceUSD, unit string, weiPerUSD *big.Rat) (*big.Int, string, error) {
	price, ok := new(big.Rat).SetString(strings.TrimSpace(priceUSD))
	if !ok || price.Sign() <= 0 || weiPerUSD == nil || weiPerUSD.Sign() <= 0 {
		return nil, "", errors.New("positive USD price and wei-per-usd rate are required")
	}
	price.Mul(price, weiPerUSD)
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "hour":
		price.Quo(price, big.NewRat(3600, 1))
		unit = "seconds"
	case "fixed":
		unit = "fixed"
	default:
		return nil, "", errors.New("payment unit must be hour or fixed")
	}
	wei := new(big.Int).Quo(price.Num(), price.Denom())
	if new(big.Int).Rem(price.Num(), price.Denom()).Sign() != 0 {
		wei.Add(wei, big.NewInt(1))
	}
	if wei.Sign() <= 0 || !wei.IsInt64() {
		return nil, "", errors.New("converted wei price exceeds supported range")
	}
	return wei, unit, nil
}
