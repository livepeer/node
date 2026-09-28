package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/internal/destination"
	"github.com/livepeer/node/internal/version"
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
	ChainID     string   `name:"chain-id" optional:"true" toml:"chain_id"`
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
	_, err = destination.New("ethereum-rpc", p.RPCGrants)
	return err
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func call(ctx context.Context, client *http.Client, endpoint, method string) (string, error) {
	data, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: []any{}})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", errors.New("invalid RPC endpoint")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("RPC request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("RPC returned HTTP %d", response.StatusCode)
	}
	var result rpcResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", errors.New("invalid RPC response")
	}
	if result.Error != nil {
		return "", fmt.Errorf("RPC error %d", result.Error.Code)
	}
	var value string
	if err := json.Unmarshal(result.Result, &value); err != nil {
		return "", errors.New("invalid RPC result")
	}
	return value, nil
}

func Status(ctx context.Context, p Params, out io.Writer) error {
	if err := p.Validate(); err != nil {
		return err
	}
	policy, err := destination.New("ethereum-rpc", p.RPCGrants)
	if err != nil {
		return err
	}
	client := policy.Client()
	client.Timeout = 15 * time.Second
	remoteID, err := call(ctx, client, p.RPCURL, "eth_chainId")
	if err != nil {
		return err
	}
	id, ok := new(big.Int).SetString(strings.TrimPrefix(remoteID, "0x"), 16)
	if !ok || id.String() != p.ChainID {
		return errors.New("RPC chain ID does not match configured chain-id")
	}
	block, err := call(ctx, client, p.RPCURL, "eth_blockNumber")
	if err != nil {
		return err
	}
	if p.Output == "json" {
		return json.NewEncoder(out).Encode(map[string]string{"chain_id": p.ChainID, "block_number": block})
	}
	_, err = fmt.Fprintf(out, "Chain ID: %s\nBlock: %s\n", p.ChainID, block)
	return err
}

func Root(out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "livepeer-chain", Short: "Direct Livepeer Ethereum management", Version: version.String(), SilenceErrors: true, SilenceUsage: true}
	root.SetOut(out)
	root.SetErr(errOut)
	status := (boa.Cmd[Params]{
		Use: "status", Short: "Read the configured Ethereum chain status", RejectUnknown: true, Args: cobra.NoArgs,
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
				for _, key := range []string{"rpc_grants", "chain_id", "output"} {
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
			return Status(cmd.Context(), *p, cmd.OutOrStdout())
		},
	}).ToCobra()
	root.AddCommand(status)
	root.InitDefaultCompletionCmd()
	return root
}
