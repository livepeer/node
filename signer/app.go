package signer

import (
	"context"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

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
	ConfigFile      string   `name:"config" configfile:"true" file:"true" optional:"true" toml:"-"`
	Listen          string   `name:"listen" default:"127.0.0.1:8937" toml:"listen"`
	MetricsListen   string   `name:"metrics-listen" default:"127.0.0.1:8938" toml:"metrics_listen"`
	RPCURL          string   `name:"rpc-url" secret:"true" optional:"true" toml:"rpc_url"`
	RPCURLFile      string   `name:"rpc-url-file" secretfor:"RPCURL" toml:"rpc_url_file"`
	RPCGrants       []string `name:"rpc-grants" optional:"true" toml:"rpc_grants"`
	RPCCAFile       string   `name:"rpc-ca-file" optional:"true" file:"true" toml:"rpc_ca_file"`
	ChainID         string   `name:"chain-id" optional:"true" toml:"chain_id"`
	Controller      string   `name:"controller-address" optional:"true" toml:"controller_address"`
	AuthToken       string   `name:"auth-token" secret:"true" optional:"true" toml:"auth_token"`
	AuthTokenFile   string   `name:"auth-token-file" secretfor:"AuthToken" toml:"auth_token_file"`
	KeyFile         string   `name:"private-key-file" file:"true" optional:"true" toml:"private_key_file"`
	StateDB         string   `name:"state-db" default:"signer.sqlite" toml:"state_db"`
	Orchestrators   []string `name:"orchestrators" optional:"true" toml:"orchestrators"`
	DiscoveryGrants []string `name:"discovery-grants" optional:"true" toml:"discovery_grants"`
	DiscoveryCAFile string   `name:"discovery-ca-file" optional:"true" file:"true" toml:"discovery_ca_file"`
	BehindTLS       bool     `name:"behind-tls" optional:"true" toml:"behind_tls"`
	PrintConfig     bool     `name:"print-config" optional:"true" boa:"noconfig" toml:"-"`
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
	if p.KeyFile == "" {
		return errors.New("private-key-file is required")
	}
	if p.RPCURL != "" || p.ChainID != "" || p.Controller != "" || len(p.RPCGrants) > 0 || p.RPCCAFile != "" {
		if p.RPCURL == "" || p.ChainID == "" || !eth.ValidAddress(p.Controller) {
			return errors.New("RPC URL, chain ID and controller address must be configured together")
		}
		id, ok := new(big.Int).SetString(p.ChainID, 10)
		if !ok || id.Sign() <= 0 {
			return errors.New("chain ID must be a positive decimal integer")
		}
		if _, err := destination.ValidateURL(p.RPCURL); err != nil {
			return errors.New("invalid RPC URL")
		}
		policy, err := destination.New("signer-ethereum-rpc", p.RPCGrants)
		if err != nil {
			return err
		}
		if _, err := policy.WithCAFile(p.RPCCAFile); err != nil {
			return err
		}
	}
	if p.StateDB == "" {
		return errors.New("state-db is required")
	}
	return nil
}

func Serve(p Params) error {
	if err := p.Validate(); err != nil {
		return err
	}
	key, err := eth.OpenKeyFile(p.KeyFile)
	if err != nil {
		return err
	}
	store, err := openStateStore(p.StateDB)
	if err != nil {
		return err
	}
	defer store.close()
	service := NewService(key, store, p.AuthToken)
	if p.RPCURL != "" {
		rpc, err := eth.OpenRPC(p.RPCURL, p.RPCGrants, p.RPCCAFile)
		if err != nil {
			return err
		}
		chainID, _ := new(big.Int).SetString(p.ChainID, 10)
		if err := rpc.CheckChainID(context.Background(), chainID); err != nil {
			return err
		}
		contracts, err := eth.OpenContracts(rpc, p.Controller)
		if err != nil {
			return err
		}
		service.SetPaymentChain(eth.PaymentChain{Contracts: contracts})
	}
	if err := service.SetDiscovery(p.Orchestrators, p.DiscoveryGrants, p.DiscoveryCAFile); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", p.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	metricsListener, err := net.Listen("tcp", p.MetricsListen)
	if err != nil {
		return err
	}
	defer metricsListener.Close()
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	metricsMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ready\n")
	})
	metricsMux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, "livepeer_signer_up 1\n")
	})
	srv := &http.Server{Handler: service, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	metricsServer := &http.Server{Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	errCh := make(chan error, 2)
	go func() { errCh <- srv.Serve(listener) }()
	go func() { errCh <- metricsServer.Serve(metricsListener) }()
	select {
	case <-ctx.Done():
	case err = <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	_ = metricsServer.Shutdown(shutdown)
	return err
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
				for _, key := range []string{"listen", "metrics_listen", "behind_tls", "discovery_grants", "rpc_grants", "chain_id", "controller_address"} {
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
			return Serve(*p)
		},
	}).ToCobra()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.InitDefaultCompletionCmd()
	return cmd
}
