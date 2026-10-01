package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/eth"
)

func init() {
	boa.RegisterConfigFormat(".toml", unmarshalConfig)
	boa.RegisterConfigMarshaler(".toml", toml.Marshal)
}

func unmarshalConfig(data []byte, target any) error {
	// Detach pointers shared with Boa's CLI/env mirrors before TOML writes
	// through them. Boa restores higher-priority values after decoding.
	if p, ok := target.(*RootParams); ok {
		p.ChainID, p.Sender, p.MaxFeePerGas = nil, nil, nil
	}
	return toml.Unmarshal(data, target)
}

func Status(ctx context.Context, p OperatorParams, display DisplayOptions, out io.Writer) error {
	client, chainID, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	defer client.Close()
	blockNumber, err := client.BlockNumber(ctx)
	if err != nil {
		return err
	}
	block := hexutil.EncodeUint64(blockNumber)
	if display.Output == "json" {
		return json.NewEncoder(out).Encode(map[string]string{"chain_id": chainID.String(), "block_number": block})
	}
	_, err = fmt.Fprintf(out, "Chain ID: %s\nBlock: %s\n", chainID, block)
	return err
}

func checkedClient(ctx context.Context, p OperatorParams) (*eth.RPC, *big.Int, error) {
	if err := p.Validate(); err != nil {
		return nil, nil, err
	}
	rpc, err := eth.NewRPC(p.RPCURL, nil)
	if err != nil {
		return nil, nil, err
	}
	id, err := rpc.ChainID(ctx)
	if err != nil {
		rpc.Close()
		return nil, nil, err
	}
	if p.ChainID != nil && id.Cmp(new(big.Int).SetUint64(*p.ChainID)) != 0 {
		rpc.Close()
		return nil, nil, errors.New("RPC chain ID does not match configured chain-id")
	}
	return rpc, id, nil
}

func Account(ctx context.Context, p OperatorParams, display DisplayOptions, out io.Writer) error {
	sender, err := p.senderAddress()
	if err != nil {
		return err
	}
	client, _, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	defer client.Close()
	balance, err := client.BalanceAt(ctx, sender, nil)
	if err != nil {
		return err
	}
	nonce, err := client.PendingNonceAt(ctx, sender)
	if err != nil {
		return err
	}
	if display.Output == "json" {
		return json.NewEncoder(out).Encode(map[string]any{"address": sender.Hex(), "balance_wei": balance.String(), "nonce": nonce})
	}
	_, err = fmt.Fprintf(out, "Address: %s\nETH balance (wei): %s\nPending nonce: %d\n", sender.Hex(), balance.String(), nonce)
	return err
}
