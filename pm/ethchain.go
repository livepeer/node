package pm

import (
	"context"
	"math/big"

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

func (c EthereumChain) ValidateSender(ctx context.Context, sender ethcommon.Address, faceValue *big.Int) error {
	return c.Client.ValidateSender(ctx, sender, faceValue)
}

func (c EthereumChain) IsActive(ctx context.Context, recipient ethcommon.Address) (bool, error) {
	return c.Client.IsActive(ctx, recipient)
}

var _ PaymentChain = EthereumChain{}
