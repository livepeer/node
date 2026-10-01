package chain

import (
	"errors"
	"io"
	"net/url"

	"github.com/BurntSushi/toml"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/destination"
)

// OperatorParams is the persistent operator context shared by chain commands.
type OperatorParams struct {
	RPCURL       *url.URL           `name:"rpc-url" secret:"true" optional:"true" persistent:"true"`
	RPCURLFile   string             `name:"rpc-url-file" secretfor:"RPCURL" persistent:"true"`
	ChainID      *uint64            `min:"1" persistent:"true" descr:"Optional expected RPC chain ID"`
	Sender       *ethcommon.Address `persistent:"true"`
	Controller   ethcommon.Address  `name:"controller-address" required:"true" persistent:"true" default:"0xD8E8328501E9645d16Cf49539efC04f734606ee4" descr:"Livepeer Controller (Arbitrum mainnet)"`
	KeyFile      string             `name:"private-key-file" optional:"true" file:"true" persistent:"true"`
	MaxFeePerGas *uint256.Int       `persistent:"true" descr:"Optional maximum transaction fee in wei per gas"`
}

func (p OperatorParams) Validate() error {
	if err := destination.ValidateURL(p.RPCURL); err != nil {
		return errors.New("valid RPC URL is required")
	}
	if p.Controller == (ethcommon.Address{}) {
		return errors.New("controller-address must be nonzero")
	}
	if p.MaxFeePerGas != nil && p.MaxFeePerGas.IsZero() {
		return errors.New("max-fee-per-gas must be positive")
	}
	return nil
}

// printConfig deliberately includes only audited operator values.
func (p OperatorParams) printConfig(out io.Writer) error {
	data, err := toml.Marshal(struct {
		ChainID      *uint64
		Sender       *ethcommon.Address
		Controller   ethcommon.Address
		MaxFeePerGas *uint256.Int
	}{p.ChainID, p.Sender, p.Controller, p.MaxFeePerGas})
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

// DisplayOptions are invocation controls, never persistent configuration.
type DisplayOptions struct {
	Output      string `default:"text" alts:"text,json" persistent:"true" boa:"noconfig,noenv"`
	PrintConfig bool   `optional:"true" persistent:"true" boa:"noconfig,noenv" descr:"Print audited operator configuration and exit"`
}

type RootParams struct {
	ConfigFile string `name:"config" configfile:"true" file:"true" optional:"true" persistent:"true" boa:"noconfig"`
	OperatorParams
	DisplayOptions
}

// TransactionOptions belongs only to commands that can change on-chain state.
type TransactionOptions struct {
	Submit bool `optional:"true" boa:"noconfig,noenv" descr:"Broadcast the simulated transaction"`
	Wait   bool `optional:"true" boa:"noconfig,noenv" descr:"Wait for a successful receipt (requires --submit)"`
	Quiet  bool `optional:"true" boa:"noconfig,noenv" descr:"Suppress normal transaction output"`
}

func (p TransactionOptions) transactionOptions() TransactionOptions { return p }

type PositiveAmountOptions struct {
	Amount uint256.Int `required:"true" boa:"noconfig,noenv" descr:"Positive integer amount in token base units or wei"`
}

func (p *PositiveAmountOptions) InitCtx(ctx *boa.HookContext) error {
	boa.Param(ctx, &p.Amount).SetCustomValidator(func(amount uint256.Int) error {
		if amount.IsZero() {
			return errors.New("amount must be positive")
		}
		return nil
	})
	return nil
}

type LockOptions struct {
	LockID uint256.Int `required:"true" boa:"noconfig,noenv"`
}

type PercentageOptions struct {
	RewardCut *uint64 `max:"1000000" boa:"noconfig,noenv"`
	FeeShare  *uint64 `max:"1000000" boa:"noconfig,noenv"`
}

type ActivateParams struct {
	TransactionOptions
	PercentageOptions
}

func (p *ActivateParams) InitCtx(ctx *boa.HookContext) error {
	boa.Param(ctx, &p.RewardCut).SetRequired(true)
	boa.Param(ctx, &p.FeeShare).SetRequired(true)
	return nil
}

type SetConfigParams struct {
	TransactionOptions
	PercentageOptions
	ServiceURI boa.Text[*url.URL] `optional:"true" boa:"noconfig,noenv"`
}

type BondParams struct {
	TransactionOptions
	PositiveAmountOptions
	Orchestrator ethcommon.Address `positional:"true" required:"true" boa:"noconfig,noenv" descr:"Ethereum address of the target orchestrator"`
}

type UnbondParams struct {
	TransactionOptions
	PositiveAmountOptions
}

type RebondParams struct {
	TransactionOptions
	LockOptions
	Delegate *ethcommon.Address `boa:"noconfig,noenv"`
}

type WithdrawStakeParams struct {
	TransactionOptions
	LockOptions
}

type ClaimParams struct {
	TransactionOptions
	EndRound uint256.Int `required:"true" boa:"noconfig,noenv"`
}

type WithdrawFeesParams struct {
	TransactionOptions
	PositiveAmountOptions
	Recipient ethcommon.Address `required:"true" boa:"noconfig,noenv"`
}

type FundParams struct {
	TransactionOptions
	Amount  uint256.Int `required:"true" boa:"noconfig,noenv" descr:"Deposit amount in wei"`
	Reserve uint256.Int `required:"true" boa:"noconfig,noenv" descr:"Reserve amount in wei"`
}
