package signer

import (
	"errors"
	"io"
	"net"

	"github.com/BurntSushi/toml"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/internal/version"
	"github.com/spf13/cobra"
)

func init() {
	boa.RegisterConfigFormat(".toml", toml.Unmarshal)
	boa.RegisterConfigMarshaler(".toml", toml.Marshal)
}

type Params struct {
	ConfigFile    string `name:"config" configfile:"true" file:"true" optional:"true" toml:"-"`
	Listen        string `name:"listen" default:"127.0.0.1:8937" toml:"listen"`
	MetricsListen string `name:"metrics-listen" default:"127.0.0.1:8938" toml:"metrics_listen"`
	RPCURL        string `name:"rpc-url" secret:"true" optional:"true" toml:"rpc_url"`
	RPCURLFile    string `name:"rpc-url-file" secretfor:"RPCURL" toml:"rpc_url_file"`
	AuthToken     string `name:"auth-token" secret:"true" optional:"true" toml:"auth_token"`
	AuthTokenFile string `name:"auth-token-file" secretfor:"AuthToken" toml:"auth_token_file"`
	BehindTLS     bool   `name:"behind-tls" optional:"true" toml:"behind_tls"`
	PrintConfig   bool   `name:"print-config" optional:"true" boa:"noconfig" toml:"-"`
}

func (p Params) Validate() error {
	host, _, err := net.SplitHostPort(p.Listen)
	if err != nil {
		return errors.New("listen must be host:port")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("listen must use a literal IP address")
	}
	if !ip.IsLoopback() && (!p.BehindTLS || p.AuthToken == "") {
		return errors.New("non-loopback signer requires behind-tls and authentication")
	}
	metricsHost, _, err := net.SplitHostPort(p.MetricsListen)
	if err != nil {
		return errors.New("metrics-listen must be host:port")
	}
	metricsIP := net.ParseIP(metricsHost)
	if metricsIP == nil || !metricsIP.IsLoopback() {
		return errors.New("metrics listener must bind loopback")
	}
	return nil
}

func Root(out, errOut io.Writer) *cobra.Command {
	cmd := (boa.Cmd[Params]{
		Use: "livepeer-signer", Short: "Standalone remote signer", Version: version.String(),
		RejectUnknown: true,
		ParamEnrich:   boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_SIGNER")),
		Args:          cobra.NoArgs,
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
				for _, key := range []string{"listen", "metrics_listen", "behind_tls"} {
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
			if err := p.Validate(); err != nil {
				return err
			}
			return errors.New("remote signer payment implementation is pending; no signer listener started")
		},
	}).ToCobra()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.InitDefaultCompletionCmd()
	return cmd
}
