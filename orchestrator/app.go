package orchestrator

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/version"
	"github.com/spf13/cobra"
)

func init() {
	boa.RegisterConfigFormat(".toml", toml.Unmarshal)
	boa.RegisterConfigMarshaler(".toml", toml.Marshal)
}

type Params struct {
	ConfigFile          string        `name:"config" configfile:"true" file:"true" optional:"true" toml:"-" descr:"TOML configuration path"`
	Listen              string        `name:"listen" default:"127.0.0.1:8935" toml:"listen" descr:"Public HTTP listener"`
	MetricsListen       string        `name:"metrics-listen" default:"127.0.0.1:8936" toml:"metrics_listen" descr:"Loopback metrics listener"`
	ServiceURL          string        `name:"service-url" default:"http://127.0.0.1:8935" toml:"service_url" descr:"Public orchestrator base URL"`
	BootstrapSecret     string        `name:"bootstrap-secret" secret:"true" optional:"true" toml:"bootstrap_secret" descr:"Dynamic runner bootstrap credential"`
	BootstrapSecretFile string        `name:"bootstrap-secret-file" secretfor:"BootstrapSecret" toml:"bootstrap_secret_file" descr:"File containing runner bootstrap credential"`
	RunnerConfig        string        `name:"runner-config" optional:"true" file:"true" toml:"runner_config" descr:"Static runner TOML path"`
	PaymentDB           string        `name:"payment-db" optional:"true" toml:"payment_db"`
	PaymentKeyFile      string        `name:"payment-key-file" optional:"true" file:"true" toml:"payment_key_file"`
	PaymentRPCURL       string        `name:"payment-rpc-url" secret:"true" optional:"true" toml:"payment_rpc_url"`
	PaymentRPCURLFile   string        `name:"payment-rpc-url-file" secretfor:"PaymentRPCURL" toml:"payment_rpc_url_file"`
	PaymentRPCGrants    []string      `name:"payment-rpc-grants" optional:"true" toml:"payment_rpc_grants"`
	PaymentRPCCAFile    string        `name:"payment-rpc-ca-file" optional:"true" file:"true" toml:"payment_rpc_ca_file"`
	PaymentChainID      string        `name:"payment-chain-id" optional:"true" toml:"payment_chain_id"`
	PaymentController   string        `name:"payment-controller-address" optional:"true" toml:"payment_controller_address"`
	WeiPerUSD           string        `name:"wei-per-usd" optional:"true" toml:"wei_per_usd"`
	TicketFaceValue     string        `name:"ticket-face-value" optional:"true" toml:"ticket_face_value"`
	TicketWinProb       string        `name:"ticket-win-prob" optional:"true" toml:"ticket_win_prob"`
	RunnerGrants        []string      `name:"runner-grants" optional:"true" toml:"runner_grants" descr:"Exact private runner host:port grants"`
	RunnerCAFile        string        `name:"runner-ca-file" optional:"true" file:"true" toml:"runner_ca_file" descr:"Custom runner CA bundle"`
	SessionProxyGrants  []string      `name:"session-proxy-grants" optional:"true" toml:"session_proxy_grants" descr:"Exact private generated proxy target grants"`
	SessionProxyCAFile  string        `name:"session-proxy-ca-file" optional:"true" file:"true" toml:"session_proxy_ca_file" descr:"Custom session proxy CA bundle"`
	HealthGrants        []string      `name:"health-grants" optional:"true" toml:"health_grants" descr:"Exact private static runner health grants"`
	HealthCAFile        string        `name:"health-ca-file" optional:"true" file:"true" toml:"health_ca_file" descr:"Custom static runner health CA bundle"`
	HeartbeatInterval   time.Duration `name:"heartbeat-interval" default:"5s" toml:"heartbeat_interval" descr:"Runner heartbeat interval"`
	HeartbeatTTL        time.Duration `name:"heartbeat-ttl" default:"30s" toml:"heartbeat_ttl" descr:"Runner heartbeat expiry"`
	BehindTLS           bool          `name:"behind-tls" optional:"true" toml:"behind_tls" descr:"Listener is behind an operator TLS terminator"`
	TLSCertFile         string        `name:"tls-cert-file" optional:"true" file:"true" toml:"tls_cert_file" descr:"Operator-supplied TLS certificate PEM"`
	TLSKeyFile          string        `name:"tls-key-file" optional:"true" file:"true" toml:"tls_key_file" descr:"Operator-supplied TLS private key PEM"`
	PrintConfig         bool          `name:"print-config" optional:"true" boa:"noconfig" toml:"-" descr:"Print audited redacted TOML configuration"`
}

func (p Params) Validate() error {
	if p.PrintConfig {
		return nil
	}
	if p.BootstrapSecret == "" && p.RunnerConfig == "" {
		return errors.New("bootstrap secret or static runner config is required")
	}
	paymentRequested := p.PaymentKeyFile != "" || p.PaymentDB != "" || p.PaymentRPCURL != "" || p.PaymentChainID != "" || p.PaymentController != "" || p.WeiPerUSD != "" || p.TicketFaceValue != "" || p.TicketWinProb != "" || len(p.PaymentRPCGrants) > 0 || p.PaymentRPCCAFile != ""
	if paymentRequested {
		if p.PaymentKeyFile == "" || p.PaymentDB == "" || p.PaymentRPCURL == "" || p.PaymentChainID == "" || p.PaymentController == "" || p.WeiPerUSD == "" || p.TicketFaceValue == "" || p.TicketWinProb == "" {
			return errors.New("on-chain payment requires payment-db, key, RPC, chain-id, controller, wei-per-usd, face-value and win-prob")
		}
		if !eth.ValidAddress(p.PaymentController) {
			return errors.New("invalid payment controller address")
		}
		for _, value := range []string{p.PaymentChainID, p.TicketFaceValue, p.TicketWinProb} {
			n, ok := new(big.Int).SetString(value, 10)
			if !ok || n.Sign() <= 0 {
				return errors.New("payment chain ID and ticket values must be positive decimal integers")
			}
		}
		rate, ok := new(big.Rat).SetString(p.WeiPerUSD)
		if !ok || rate.Sign() <= 0 {
			return errors.New("wei-per-usd must be positive")
		}
		if _, err := destination.ValidateURL(p.PaymentRPCURL); err != nil {
			return errors.New("invalid payment RPC URL")
		}
	}
	listenHost, _, err := net.SplitHostPort(p.Listen)
	if err != nil {
		return errors.New("listen must be host:port")
	}
	metricsHost, _, err := net.SplitHostPort(p.MetricsListen)
	if err != nil {
		return errors.New("metrics-listen must be host:port")
	}
	if !isLoopbackHost(metricsHost) {
		return errors.New("metrics listener must bind loopback")
	}
	if (p.TLSCertFile == "") != (p.TLSKeyFile == "") {
		return errors.New("tls-cert-file and tls-key-file must be configured together")
	}
	if p.TLSCertFile != "" && p.BehindTLS {
		return errors.New("direct TLS and behind-tls cannot be combined")
	}
	if p.TLSCertFile != "" {
		if _, err := tls.LoadX509KeyPair(p.TLSCertFile, p.TLSKeyFile); err != nil {
			return errors.New("invalid direct TLS certificate or private key")
		}
	}
	if !isLoopbackHost(listenHost) && !p.BehindTLS && p.TLSCertFile == "" {
		return errors.New("non-loopback listener requires direct TLS or behind-tls")
	}
	base, err := destination.ValidateURL(p.ServiceURL)
	if err != nil || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("service-url must be an absolute HTTP or HTTPS URL without credentials, query or fragment")
	}
	if p.TLSCertFile != "" && base.Scheme != "https" {
		return errors.New("service-url must use https with direct TLS")
	}
	if p.HeartbeatInterval <= 0 || p.HeartbeatTTL <= p.HeartbeatInterval {
		return errors.New("heartbeat-ttl must exceed positive heartbeat-interval")
	}
	runnerPolicy, err := destination.New("runner", p.RunnerGrants)
	if err != nil {
		return err
	}
	if _, err := runnerPolicy.WithCAFile(p.RunnerCAFile); err != nil {
		return err
	}
	proxyPolicy, err := destination.New("session-proxy", p.SessionProxyGrants)
	if err != nil {
		return err
	}
	if _, err := proxyPolicy.WithCAFile(p.SessionProxyCAFile); err != nil {
		return err
	}
	healthPolicy, err := destination.New("static-runner-health", p.HealthGrants)
	if err != nil {
		return err
	}
	if _, err := healthPolicy.WithCAFile(p.HealthCAFile); err != nil {
		return err
	}
	return nil
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func Root(out, errOut io.Writer) *cobra.Command {
	cmd := (boa.Cmd[Params]{
		Use: "livepeer-orchestrator", Short: "Standalone Live Runner orchestrator", Version: version.String(),
		RejectUnknown: true,
		ParamEnrich:   boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_ORCHESTRATOR")),
		Args:          cobra.NoArgs,
		RunFuncCtxE: func(ctx *boa.HookContext, p *Params, cmd *cobra.Command, _ []string) error {
			if p.PrintConfig {
				return printConfig(ctx, cmd.OutOrStdout())
			}
			if err := p.Validate(); err != nil {
				return err
			}
			return Serve(cmd.Context(), *p, cmd.ErrOrStderr())
		},
	}).ToCobra()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.InitDefaultCompletionCmd()
	return cmd
}

func printConfig(ctx *boa.HookContext, out io.Writer) error {
	raw, err := ctx.DumpBytes(".toml", nil)
	if err != nil {
		return err
	}
	var values map[string]any
	if err := toml.Unmarshal(raw, &values); err != nil {
		return err
	}
	// File paths and any future free-form strings are excluded by this allowlist.
	allowed := map[string]any{}
	for _, key := range []string{"listen", "metrics_listen", "runner_grants", "session_proxy_grants", "health_grants", "heartbeat_interval", "heartbeat_ttl", "behind_tls", "payment_db", "payment_chain_id", "payment_controller_address", "wei_per_usd", "ticket_face_value", "ticket_win_prob"} {
		if value, exists := values[key]; exists {
			allowed[key] = value
		}
	}
	data, err := toml.Marshal(allowed)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}

func loadStatic(path string, registry *Registry) error {
	if path == "" {
		return nil
	}
	var config struct {
		Runners []StaticRunner `toml:"runners"`
	}
	meta, err := toml.DecodeFile(path, &config)
	if err != nil {
		return err
	}
	if len(meta.Undecoded()) > 0 {
		return fmt.Errorf("unknown static runner keys: %v", meta.Undecoded())
	}
	for _, item := range config.Runners {
		if err := registry.AddStatic(item); err != nil {
			return err
		}
	}
	return nil
}

func Serve(parent context.Context, p Params, logOut io.Writer) error {
	runnerPolicy, err := destination.New("runner", p.RunnerGrants)
	if err != nil {
		return err
	}
	runnerPolicy, err = runnerPolicy.WithCAFile(p.RunnerCAFile)
	if err != nil {
		return err
	}
	proxyPolicy, err := destination.New("session-proxy", p.SessionProxyGrants)
	if err != nil {
		return err
	}
	proxyPolicy, err = proxyPolicy.WithCAFile(p.SessionProxyCAFile)
	if err != nil {
		return err
	}
	healthPolicy, err := destination.New("static-runner-health", p.HealthGrants)
	if err != nil {
		return err
	}
	healthPolicy, err = healthPolicy.WithCAFile(p.HealthCAFile)
	if err != nil {
		return err
	}
	registry := NewRegistry(p.BootstrapSecret, p.ServiceURL, p.HeartbeatInterval, p.HeartbeatTTL)
	var engine *pm.Engine
	var paymentStore *pm.SQLiteStore
	var paymentChain eth.PaymentChain
	var paymentKey *eth.Key
	var paymentChainID *big.Int
	if p.PaymentKeyFile != "" {
		rate, _ := new(big.Rat).SetString(p.WeiPerUSD)
		registry.SetWeiPerUSD(rate)
		paymentKey, err = eth.OpenKeyFile(p.PaymentKeyFile)
		if err != nil {
			return err
		}
		paymentStore, err = pm.OpenSQLite(p.PaymentDB)
		if err != nil {
			return err
		}
		defer paymentStore.Close()
		rpc, err := eth.OpenRPC(p.PaymentRPCURL, p.PaymentRPCGrants, p.PaymentRPCCAFile)
		if err != nil {
			return err
		}
		paymentChainID, _ = new(big.Int).SetString(p.PaymentChainID, 10)
		if err := rpc.CheckChainID(parent, paymentChainID); err != nil {
			return err
		}
		contracts, err := eth.OpenContracts(rpc, p.PaymentController)
		if err != nil {
			return err
		}
		paymentChain = eth.PaymentChain{Contracts: contracts}
		face, _ := new(big.Int).SetString(p.TicketFaceValue, 10)
		prob, _ := new(big.Int).SetString(p.TicketWinProb, 10)
		engine, err = pm.NewEngine(paymentStore, pm.EthereumChain{Client: paymentChain}, paymentKey.Address(), face, prob)
		if err != nil {
			return err
		}
	}
	if err := loadStatic(p.RunnerConfig, registry); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(logOut, nil))
	app := NewServer(registry, runnerPolicy, proxyPolicy, logger)
	app.SetPayment(engine)
	sem := make(chan struct{}, 256)
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-r.Context().Done():
			return
		default:
			fail(w, http.StatusServiceUnavailable, "server busy")
			return
		}
		app.ServeHTTP(w, r)
	})
	mainListener, err := net.Listen("tcp", p.Listen)
	if err != nil {
		return err
	}
	defer mainListener.Close()
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
		runners, sessions := registry.Counts()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintf(w, "livepeer_runners %d\nlivepeer_sessions %d\n", runners, sessions)
	})
	mainServer := &http.Server{Handler: limited, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	metricsServer := &http.Server{Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 16}
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	errs := make(chan error, 2)
	go func() {
		if p.TLSCertFile != "" {
			errs <- mainServer.ServeTLS(mainListener, p.TLSCertFile, p.TLSKeyFile)
		} else {
			errs <- mainServer.Serve(mainListener)
		}
	}()
	go func() { errs <- metricsServer.Serve(metricsListener) }()
	logger.Info("orchestrator started", "listen", p.Listen, "metrics_listen", p.MetricsListen)
	ticker := time.NewTicker(p.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
			defer done()
			_ = mainServer.Shutdown(shutdownCtx)
			_ = metricsServer.Shutdown(shutdownCtx)
			return nil
		case err := <-errs:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		case <-ticker.C:
			registry.Expire()
			if engine != nil {
				app.ChargePaidSessions(ctx)
				app.RedeemWinningTickets(ctx, paymentStore, paymentChain, paymentKey, paymentChainID)
			}
			if p.RunnerConfig != "" {
				client := healthPolicy.Client()
				client.Timeout = 5 * time.Second
				registry.CheckStaticHealth(client)
			}
		}
	}
}
