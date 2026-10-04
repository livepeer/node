package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/livepeer/node/eth"
)

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

func Account(ctx context.Context, p OperatorParams, d DisplayOptions, out io.Writer) error {
	return accountRead(ctx, p, d, out, func(s *eth.Inspection, a ethcommon.Address) (any, error) { return s.Account(ctx, a) })
}
