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
	"github.com/livepeer/node/pm"
	"github.com/spf13/cobra"
)

type redemptionParams struct {
	PaymentDB  string          `file:"true" descr:"Existing recipient SQLite database"`
	Retry      *ethcommon.Hash `name:"retry-transaction" descr:"Rebroadcast the exact stored bytes for this transaction hash"`
	Submit     bool            `optional:"true" boa:"noconfig,noenv" descr:"Explicitly authorize rebroadcast"`
	RPCURL     *url.URL        `name:"rpc-url" secret:"true" optional:"true"`
	RPCURLFile string          `name:"rpc-url-file" secretfor:"RPCURL"`
	ChainID    *uint64         `min:"1"`
}

func (p redemptionParams) PreValidate() error {
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
	return (boa.Cmd[redemptionParams]{
		Use: "redemptions", Short: "Inspect recipient redemption state or explicitly retry stored transaction bytes",
		RejectUnknown: true, Args: cobra.NoArgs,
		ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_ORCHESTRATOR")),
		RunFuncE: func(p *redemptionParams, cmd *cobra.Command, _ []string) error {
			store, err := pm.OpenSQLite(p.PaymentDB)
			if err != nil {
				return err
			}
			defer store.Close()
			if p.Retry != nil {
				rpc, err := eth.NewRPC(p.RPCURL, nil)
				if err != nil {
					return err
				}
				defer rpc.Close()
				if err := rpc.CheckChainID(cmd.Context(), new(big.Int).SetUint64(*p.ChainID)); err != nil {
					return err
				}
				err = pm.RetryRedemption(cmd.Context(), store, &eth.Contracts{RPC: rpc}, *p.Retry)
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
