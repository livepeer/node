package eth

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth/contracts/chainlink"
)

// WeiPerUSD reads an operator-selected Chainlink-compatible ETH/USD feed.
// It returns the last instant at which that observation may price new sessions.
func (c *Contracts) WeiPerUSD(ctx context.Context, feed ethcommon.Address, maxAge time.Duration) (*big.Rat, time.Time, error) {
	header, err := c.RPC.header(ctx, "latest")
	if err != nil {
		return nil, time.Time{}, err
	}
	if header.Hash == (ethcommon.Hash{}) {
		return nil, time.Time{}, errors.New("invalid price feed block")
	}
	ref := map[string]any{"blockHash": header.Hash.Hex(), "requireCanonical": true}
	reader, err := chainlink.NewAggregatorV3InterfaceCaller(feed, c.callerAt(ref))
	if err != nil {
		return nil, time.Time{}, err
	}
	opts := &bind.CallOpts{Context: ctx}
	description, err := reader.Description(opts)
	if err != nil {
		return nil, time.Time{}, err
	}
	if strings.ReplaceAll(description, " ", "") != "ETH/USD" {
		return nil, time.Time{}, errors.New("price feed must quote ETH/USD")
	}
	decimals, err := reader.Decimals(opts)
	if err != nil {
		return nil, time.Time{}, err
	}
	data, err := reader.LatestRoundData(opts)
	if err != nil {
		return nil, time.Time{}, err
	}
	round, answer, updated, answered := data.RoundId, data.Answer, data.UpdatedAt, data.AnsweredInRound
	if round.Sign() <= 0 || answer.Sign() <= 0 || !updated.IsInt64() || updated.Sign() <= 0 || answered.Cmp(round) < 0 || maxAge <= 0 {
		return nil, time.Time{}, errors.New("invalid price feed observation")
	}
	observed := time.Unix(updated.Int64(), 0)
	until := observed.Add(maxAge)
	if observed.After(time.Now()) || !time.Now().Before(until) {
		return nil, time.Time{}, errors.New("price feed observation is stale or from the future")
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18+int64(decimals)), nil)
	return new(big.Rat).SetFrac(scale, answer), until, nil
}
