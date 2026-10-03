package chain

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// parseUnits accepts unsigned decimal notation and never passes through floats.
func parseUnits(raw string, places int) (*big.Int, error) {
	if raw == "" {
		return nil, errors.New("amount is required")
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 || parts[0] == "" {
		return nil, errors.New("expected an unsigned decimal amount")
	}
	for _, part := range parts {
		if part == "" {
			return nil, errors.New("expected an unsigned decimal amount")
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return nil, errors.New("expected an unsigned decimal amount")
			}
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if len(fraction) > places {
		return nil, fmt.Errorf("amount supports at most %d decimal places", places)
	}
	digits := parts[0] + fraction + strings.Repeat("0", places-len(fraction))
	value, ok := new(big.Int).SetString(digits, 10)
	if !ok || value.BitLen() > 256 {
		return nil, errors.New("amount exceeds uint256")
	}
	return value, nil
}
func parseAmount(raw string, baseUnits, allowAll, positive bool) (*big.Int, error) {
	if raw == "all" {
		if allowAll {
			return nil, nil
		}
		return nil, errors.New("all is not supported for this amount")
	}
	places := 18
	if baseUnits {
		places = 0
	}
	value, err := parseUnits(raw, places)
	if err != nil {
		return nil, err
	}
	if positive && value.Sign() == 0 {
		return nil, errors.New("amount must be positive")
	}
	return value, nil
}
func parsePercent(raw string) (*big.Int, error) {
	value, err := parseUnits(raw, 4)
	if err != nil {
		return nil, err
	}
	if value.Cmp(big.NewInt(1000000)) > 0 {
		return nil, errors.New("percentage must be between 0 and 100")
	}
	return value, nil
}
