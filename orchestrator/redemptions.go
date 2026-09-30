package orchestrator

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm"
	"github.com/spf13/cobra"
)

type redemptionParams struct {
	PaymentDB  string   `name:"payment-db" file:"true" descr:"Existing recipient SQLite database"`
	Retry      string   `name:"retry-transaction" optional:"true" descr:"Rebroadcast the exact stored bytes for this transaction hash"`
	Submit     bool     `name:"submit" optional:"true" descr:"Explicitly authorize rebroadcast"`
	RPCURL     string   `name:"rpc-url" secret:"true" optional:"true"`
	RPCURLFile string   `name:"rpc-url-file" secretfor:"RPCURL"`
	RPCGrants  []string `name:"rpc-grants" optional:"true"`
	RPCCAFile  string   `name:"rpc-ca-file" file:"true" optional:"true"`
	ChainID    string   `name:"chain-id" optional:"true"`
}

func redemptionCommand() *cobra.Command {
	return (boa.Cmd[redemptionParams]{
		Use: "redemptions", Short: "Inspect recipient redemption state or explicitly retry stored transaction bytes",
		RejectUnknown: true, Args: cobra.NoArgs,
		RunFuncCtxE: func(_ *boa.HookContext, p *redemptionParams, cmd *cobra.Command, _ []string) error {
			if _, err := os.Stat(p.PaymentDB); err != nil {
				return errors.New("existing payment database required")
			}
			store, err := pm.OpenSQLite(p.PaymentDB)
			if err != nil {
				return err
			}
			defer store.Close()
			if p.Retry != "" {
				if !p.Submit || !ethcommon.IsHexHash(p.Retry) {
					return errors.New("retry requires a transaction hash and --submit")
				}
				chainID, ok := new(big.Int).SetString(p.ChainID, 10)
				if !ok || chainID.Sign() <= 0 {
					return errors.New("retry requires a positive chain-id")
				}
				rpc, err := eth.OpenRPC(p.RPCURL, p.RPCGrants, p.RPCCAFile)
				if err != nil {
					return err
				}
				if err := rpc.CheckChainID(cmd.Context(), chainID); err != nil {
					return err
				}
				err = pm.RetryRedemption(cmd.Context(), store, &eth.Contracts{RPC: rpc}, ethcommon.HexToHash(p.Retry))
				if err != nil {
					return err
				}
			} else if p.Submit {
				return errors.New("submit requires retry-transaction")
			}
			items, err := store.Redemptions()
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(items)
		},
	}).ToCobra()
}
