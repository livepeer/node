package orchestrator

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/url"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/nodeconfig"
	"github.com/spf13/cobra"
)

type redemptionParams struct {
	nodeconfig.Settings
	ConfigFile string          `name:"config" configfile:"optional-default" default:"orchestrator/config.toml" boa:"noconfig"`
	RedeemerDB string          `file:"true" default:"orchestrator/payments.sqlite" descr:"Existing recipient SQLite database"`
	Retry      *ethcommon.Hash `name:"retry-transaction" boa:"noconfig" descr:"Rebroadcast the exact stored bytes for this transaction hash"`
	Submit     bool            `optional:"true" boa:"noconfig,noenv" descr:"Explicitly authorize rebroadcast"`
	RPCURL     *url.URL        `name:"rpc-url" secret:"true" optional:"true"`
	RPCURLFile string          `name:"rpc-url-file" secretfor:"RPCURL"`
	ChainID    *uint64         `min:"1"`
}

func (p redemptionParams) validateRetry() error {
	if p.Retry != nil {
		if !p.Submit {
			return boa.NewUserInputError(errors.New("retry requires --submit"))
		}
		if p.ChainID == nil {
			return boa.NewUserInputError(errors.New("retry requires chain-id"))
		}
		if err := destination.ValidateURL(p.RPCURL); err != nil {
			return boa.NewUserInputError(errors.New("retry requires a valid RPC URL"))
		}
	} else if p.Submit {
		return boa.NewUserInputError(errors.New("submit requires retry-transaction"))
	}
	return nil
}

func redemptionCommand() *cobra.Command {
	params := new(redemptionParams)
	return nodeconfig.Command("orchestrator", "offchain", boa.Cmd[redemptionParams]{
		Use: "redemptions", Short: "Inspect recipient redemption state or explicitly retry stored transaction bytes",
		Params: params, Args: cobra.NoArgs,
		InitFuncCtx: func(ctx *boa.HookContext, p *redemptionParams, _ *cobra.Command) error {
			retry := func() bool { return ctx.HasValue(&p.Retry) }
			boa.Param(ctx, &p.RPCURL).SetIsEnabledFn(retry)
			boa.Param(ctx, &p.RPCURLFile).SetIsEnabledFn(retry)
			return nil
		},
		PreValidateFunc: func(p *redemptionParams, _ *cobra.Command, _ []string) error {
			if p.ChainID == nil && p.Network == nodeconfig.Mainnet {
				p.ChainID = new(uint64(42161))
			}
			if err := p.validateRetry(); err != nil {
				return err
			}
			if p.Retry != nil && p.Network == "offchain" {
				return boa.NewUserInputError(errors.New("retry requires an on-chain network"))
			}
			return nil
		},
		RunFuncE: func(p *redemptionParams, cmd *cobra.Command, _ []string) error {
			var rpc *eth.RPC
			if p.Retry != nil {
				var err error
				rpc, err = eth.NewRPC(p.RPCURL, nil)
				if err != nil {
					return err
				}
				defer rpc.Close()
				if err := rpc.CheckChainID(cmd.Context(), new(big.Int).SetUint64(*p.ChainID)); err != nil {
					return err
				}
			}
			store, err := OpenRedeemerDB(p.RedeemerDB)
			if err != nil {
				return err
			}
			defer store.Close()
			if p.Retry != nil {
				err = RetryRedemption(cmd.Context(), store, &eth.Contracts{RPC: rpc}, *p.Retry)
				if err != nil {
					return err
				}
			}
			items, err := store.Redemptions()
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
		},
	}).ToCobra()
}
