package pm

import (
	"context"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
)

// EthereumChain adapts retained Ethereum contract reads to the payment
// engine's interface. Contract ABI and RPC logic remain in eth.
type EthereumChain struct{ Client eth.PaymentChain }

func (c EthereumChain) Snapshot(ctx context.Context) (ChainSnapshot, error) {
	state, err := c.Client.Snapshot(ctx)
	if err != nil {
		return ChainSnapshot{}, err
	}
	return state, nil
}

func (c EthereumChain) SenderInfo(ctx context.Context, sender, recipient ethcommon.Address) (eth.SenderInfo, error) {
	return c.Client.SenderInfo(ctx, sender, recipient)
}

func (c EthereumChain) IsActiveAt(ctx context.Context, recipient ethcommon.Address, snapshot ChainSnapshot) (bool, error) {
	return c.Client.IsActiveAt(ctx, recipient, snapshot)
}

var _ PaymentChain = EthereumChain{}
