package eth

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

// WeiPerUSD reads an operator-selected Chainlink-compatible ETH/USD feed.
// It returns the last instant at which that observation may price new sessions.
func (c *Contracts) WeiPerUSD(ctx context.Context, feed ethcommon.Address, maxAge time.Duration) (*big.Rat, time.Time, error) {
	raw, err := c.RPC.Call(ctx, "eth_getBlockByNumber", "latest", false)
	if err != nil {
		return nil, time.Time{}, err
	}
	var header struct {
		Hash string `json:"hash"`
	}
	if json.Unmarshal(raw, &header) != nil || !ethcommon.IsHexHash(header.Hash) {
		return nil, time.Time{}, errors.New("invalid price feed block")
	}
	ref := map[string]any{"blockHash": header.Hash, "requireCanonical": true}
	call := func(method string) ([]any, error) { return c.CallAt(ctx, ref, "priceFeed", feed, method) }
	description, err := call("description")
	if err != nil {
		return nil, time.Time{}, err
	}
	if strings.ReplaceAll(description[0].(string), " ", "") != "ETH/USD" {
		return nil, time.Time{}, errors.New("price feed must quote ETH/USD")
	}
	decimals, err := call("decimals")
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := call("latestRoundData")
	if err != nil {
		return nil, time.Time{}, err
	}
	round, answer, updated, answered := data[0].(*big.Int), data[1].(*big.Int), data[3].(*big.Int), data[4].(*big.Int)
	if round.Sign() <= 0 || answer.Sign() <= 0 || !updated.IsInt64() || updated.Sign() <= 0 || answered.Cmp(round) < 0 || maxAge <= 0 {
		return nil, time.Time{}, errors.New("invalid price feed observation")
	}
	observed := time.Unix(updated.Int64(), 0)
	until := observed.Add(maxAge)
	if observed.After(time.Now()) || !time.Now().Before(until) {
		return nil, time.Time{}, errors.New("price feed observation is stale or from the future")
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18+int64(decimals[0].(uint8))), nil)
	return new(big.Rat).SetFrac(scale, answer), until, nil
}
