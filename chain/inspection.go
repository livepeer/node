package chain

import (
	"context"
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
)

func inspectRead(ctx context.Context, p OperatorParams, display DisplayOptions, out io.Writer, run func(*eth.Inspection) (any, error)) error {
	rpc, _, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	defer rpc.Close()
	c, err := eth.NewContracts(rpc, p.Controller)
	if err != nil {
		return err
	}
	s, err := c.Inspect(ctx)
	if err != nil {
		return err
	}
	value, err := run(s)
	if err != nil {
		return err
	}
	return formatResult(out, display.Output, value)
}
func orchestratorGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return accountRead(ctx, p, d, out, func(s *eth.Inspection, a common.Address) (any, error) {
		return s.Orchestrator(ctx, a)
	})
}
func stakeGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return accountRead(ctx, p, d, out, func(s *eth.Inspection, a common.Address) (any, error) {
		return s.Stake(ctx, a)
	})
}
func roundGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return inspectRead(ctx, p, d, out, func(s *eth.Inspection) (any, error) { return s.Round(ctx) })
}
func protocolGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return inspectRead(ctx, p, d, out, func(s *eth.Inspection) (any, error) { return s.Protocol(ctx) })
}
func contractsGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return inspectRead(ctx, p, d, out, func(s *eth.Inspection) (any, error) { return s.Addresses(ctx) })
}
func ticketBrokerGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return accountRead(ctx, p, d, out, func(s *eth.Inspection, a common.Address) (any, error) {
		return s.TicketBroker(ctx, a)
	})
}
func rewardCallerGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return accountRead(ctx, p, d, out, func(s *eth.Inspection, a common.Address) (any, error) {
		caller, err := s.Call(ctx, "bondingManager", "transcoderToRewardCaller", a)
		if err != nil {
			return nil, err
		}
		return struct {
			eth.BlockSnapshot
			Address      common.Address `json:"address"`
			RewardCaller common.Address `json:"reward_caller"`
		}{s.Snapshot(), a, caller[0].(common.Address)}, nil
	})
}

func accountRead(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer, run func(*eth.Inspection, common.Address) (any, error)) error {
	address, err := p.account()
	if err != nil {
		return err
	}
	return inspectRead(ctx, p, d, out, func(s *eth.Inspection) (any, error) { return run(s, address) })
}

func gasGet(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer, options GasParams) error {
	rpc, _, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	defer rpc.Close()
	c, err := eth.NewContracts(rpc, p.Controller)
	if err != nil {
		return err
	}
	if p.MaxFeePerGas != nil {
		c.MaxFeePerGas = p.MaxFeePerGas.ToBig()
	}
	var tip *big.Int
	if options.MaxPriorityFeePerGas != nil {
		tip = options.MaxPriorityFeePerGas.ToBig()
	}
	fees, err := c.InspectFees(ctx, tip)
	if err != nil {
		return err
	}
	return formatResult(out, d.Output, fees)
}
