package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/version"
	"github.com/spf13/cobra"
)

func init() {
	boa.RegisterConfigFormat(".toml", toml.Unmarshal)
	boa.RegisterConfigMarshaler(".toml", toml.Marshal)
}

type Params struct {
	ConfigFile  string   `name:"config" configfile:"true" file:"true" optional:"true" toml:"-"`
	RPCURL      string   `name:"rpc-url" secret:"true" optional:"true" toml:"rpc_url"`
	RPCURLFile  string   `name:"rpc-url-file" secretfor:"RPCURL" toml:"rpc_url_file"`
	RPCGrants   []string `name:"rpc-grants" optional:"true" toml:"rpc_grants"`
	RPCCAFile   string   `name:"rpc-ca-file" optional:"true" file:"true" toml:"rpc_ca_file"`
	ChainID     string   `name:"chain-id" optional:"true" toml:"chain_id"`
	Sender      string   `name:"sender" optional:"true" toml:"sender"`
	Controller  string   `name:"controller-address" optional:"true" toml:"controller_address"`
	KeyFile     string   `name:"private-key-file" optional:"true" file:"true" toml:"private_key_file"`
	Submit      bool     `name:"submit" optional:"true" toml:"submit"`
	Wait        bool     `name:"wait" optional:"true" toml:"wait"`
	Amount      string   `name:"amount" optional:"true" toml:"amount"`
	Reserve     string   `name:"reserve" optional:"true" toml:"reserve"`
	Delegate    string   `name:"delegate" optional:"true" toml:"delegate"`
	Recipient   string   `name:"recipient" optional:"true" toml:"recipient"`
	LockID      string   `name:"lock-id" optional:"true" toml:"lock_id"`
	EndRound    string   `name:"end-round" optional:"true" toml:"end_round"`
	RewardCut   string   `name:"reward-cut" optional:"true" toml:"reward_cut"`
	FeeShare    string   `name:"fee-share" optional:"true" toml:"fee_share"`
	ServiceURI  string   `name:"service-uri" optional:"true" toml:"service_uri"`
	Output      string   `name:"output" default:"text" toml:"output"`
	PrintConfig bool     `name:"print-config" optional:"true" boa:"noconfig" toml:"-"`
}

func (p Params) Validate() error {
	if p.RPCURL == "" {
		return errors.New("rpc-url or rpc-url-file is required")
	}
	_, err := destination.ValidateURL(p.RPCURL)
	if err != nil {
		return errors.New("invalid RPC URL")
	}
	if p.ChainID == "" {
		return errors.New("chain-id is required")
	}
	chainID, ok := new(big.Int).SetString(p.ChainID, 10)
	if !ok || chainID.Sign() <= 0 {
		return errors.New("chain-id must be a decimal integer")
	}
	if p.Output != "text" && p.Output != "json" {
		return errors.New("output must be text or json")
	}
	policy, err := destination.New("ethereum-rpc", p.RPCGrants)
	if err != nil {
		return err
	}
	_, err = policy.WithCAFile(p.RPCCAFile)
	return err
}

func Status(ctx context.Context, p Params, out io.Writer) error {
	client, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	block, err := client.CallString(ctx, "eth_blockNumber")
	if err != nil {
		return err
	}
	if p.Output == "json" {
		return json.NewEncoder(out).Encode(map[string]string{"chain_id": p.ChainID, "block_number": block})
	}
	_, err = fmt.Fprintf(out, "Chain ID: %s\nBlock: %s\n", p.ChainID, block)
	return err
}

func checkedClient(ctx context.Context, p Params) (*eth.RPC, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	rpc, err := eth.OpenRPC(p.RPCURL, p.RPCGrants, p.RPCCAFile)
	if err != nil {
		return nil, err
	}
	id, _ := new(big.Int).SetString(p.ChainID, 10)
	if err := rpc.CheckChainID(ctx, id); err != nil {
		return nil, err
	}
	return rpc, nil
}

func Account(ctx context.Context, p Params, out io.Writer) error {
	if !eth.ValidAddress(p.Sender) {
		return errors.New("sender must be a 0x-prefixed 20-byte Ethereum address")
	}
	client, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	balanceRaw, err := client.CallString(ctx, "eth_getBalance", p.Sender, "latest")
	if err != nil {
		return err
	}
	balance, err := eth.ParseHexQuantity(balanceRaw)
	if err != nil {
		return err
	}
	nonceRaw, err := client.CallString(ctx, "eth_getTransactionCount", p.Sender, "pending")
	if err != nil {
		return err
	}
	nonce, err := eth.ParseHexQuantity(nonceRaw)
	if err != nil || !nonce.IsUint64() {
		return errors.New("invalid account nonce from RPC")
	}
	if p.Output == "json" {
		return json.NewEncoder(out).Encode(map[string]any{"address": p.Sender, "balance_wei": balance.String(), "nonce": nonce.Uint64()})
	}
	_, err = fmt.Fprintf(out, "Address: %s\nETH balance (wei): %s\nPending nonce: %d\n", p.Sender, balance.String(), nonce.Uint64())
	return err
}

func Root(out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "livepeer-chain", Short: "Direct Livepeer Ethereum management", Version: version.String(), SilenceErrors: true, SilenceUsage: true}
	root.SetOut(out)
	root.SetErr(errOut)
	add := func(use, short string, run func(context.Context, Params, io.Writer) error) {
		parts := strings.Split(use, " ")
		parent := root
		for _, part := range parts[:len(parts)-1] {
			var group *cobra.Command
			for _, child := range parent.Commands() {
				if child.Name() == part {
					group = child
					break
				}
			}
			if group == nil {
				group = &cobra.Command{Use: part}
				parent.AddCommand(group)
			}
			parent = group
		}
		command := (boa.Cmd[Params]{
			Use: parts[len(parts)-1], Short: short, RejectUnknown: true, Args: cobra.NoArgs,
			ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_CHAIN")),
			RunFuncCtxE: func(ctx *boa.HookContext, p *Params, cmd *cobra.Command, _ []string) error {
				if p.PrintConfig {
					raw, err := ctx.DumpBytes(".toml", nil)
					if err != nil {
						return err
					}
					var values map[string]any
					if err := toml.Unmarshal(raw, &values); err != nil {
						return err
					}
					safe := map[string]any{}
					for _, key := range []string{"rpc_grants", "chain_id", "sender", "controller_address", "output", "submit", "wait", "amount", "reserve", "delegate", "recipient", "lock_id", "end_round", "reward_cut", "fee_share"} {
						if value, ok := values[key]; ok {
							safe[key] = value
						}
					}
					data, err := toml.Marshal(safe)
					if err != nil {
						return err
					}
					_, err = cmd.OutOrStdout().Write(data)
					return err
				}
				return run(cmd.Context(), *p, cmd.OutOrStdout())
			},
		}).ToCobra()
		parent.AddCommand(command)
	}
	add("status", "Read the configured Ethereum chain status", Status)
	add("account", "Read the sender's ETH balance and pending nonce", Account)
	addContractCommands(root, add)
	root.InitDefaultCompletionCmd()
	return root
}
