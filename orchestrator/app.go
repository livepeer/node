package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/version"
	"github.com/spf13/cobra"
)

type Params struct {
	ETHUSDFeed           *ethcommon.Address `name:"eth-usd-feed" descr:"ETH/USD oracle address, alternative to a fixed wei-per-usd rate"`
	PriceMaxAge          time.Duration      `default:"2h" descr:"Maximum age of the oracle observation"`
	ConfigFile           string             `name:"config" configfile:"true" file:"true" optional:"true" boa:"noconfig" descr:"TOML configuration path"`
	Listen               netip.AddrPort     `default:"127.0.0.1:8935" descr:"Public HTTP listener"`
	MetricsListen        netip.AddrPort     `default:"127.0.0.1:8936" descr:"Loopback metrics listener"`
	ServiceURL           boa.Text[*url.URL] `default:"http://127.0.0.1:8935" descr:"Public orchestrator base URL"`
	RunnerServiceURL     boa.Text[*url.URL] `optional:"true" descr:"Runner-facing base URL for callbacks and trickle; defaults to service-url"`
	ProxyURLTemplate     string             `optional:"true" descr:"Generated proxy URL with {proxy} in a hostname label or final path segment"`
	BootstrapSecret      string             `secret:"true" optional:"true" descr:"Dynamic runner bootstrap credential"`
	BootstrapSecretFile  string             `secretfor:"BootstrapSecret" descr:"File containing runner bootstrap credential"`
	RunnerConfig         string             `optional:"true" file:"true" descr:"Static runner TOML path"`
	PaymentDB            string             `optional:"true"`
	KeystoreFile         string             `optional:"true" descr:"Encrypted geth redemption account JSON file"`
	KeystorePasswordFile string             `optional:"true" descr:"Owner-only file containing the exact keystore password bytes"`
	PaymentRPCURL        *url.URL           `name:"payment-rpc-url" secret:"true" optional:"true"`
	PaymentRPCURLFile    string             `name:"payment-rpc-url-file" secretfor:"PaymentRPCURL"`
	PaymentChainID       *uint64            `min:"1"`
	PaymentMaxFeePerGas  *uint256.Int       `descr:"Optional maximum redemption fee in wei per gas"`
	PaymentController    *ethcommon.Address `name:"payment-controller-address"`
	WeiPerUSD            *big.Rat
	TicketFaceValue      *uint256.Int
	TicketWinProb        *uint256.Int
	RunnerGrants         []string      `optional:"true" descr:"Exact private runner host:port grants"`
	SessionProxyGrants   []string      `optional:"true" descr:"Exact private generated proxy target grants"`
	HealthGrants         []string      `optional:"true" descr:"Exact private static runner health grants"`
	HeartbeatInterval    time.Duration `default:"5s" descr:"Runner heartbeat interval"`
	HeartbeatTTL         time.Duration `default:"30s" descr:"Runner heartbeat expiry"`
	PrintConfig          bool          `optional:"true" boa:"noconfig,noenv" descr:"Print audited redacted TOML configuration"`
}

func unmarshalConfig(data []byte, target any) error {
	// Detach pointers shared with Boa's CLI/env mirrors before TOML writes
	// through them. Boa restores higher-priority values after decoding.
	if p, ok := target.(*Params); ok {
		p.PaymentChainID, p.PaymentController, p.ETHUSDFeed = nil, nil, nil
		p.PaymentMaxFeePerGas, p.TicketFaceValue, p.TicketWinProb = nil, nil, nil
		p.WeiPerUSD = nil
	}
	return toml.Unmarshal(data, target)
}

func (p Params) Validate() error {
	if p.BootstrapSecret == "" && p.RunnerConfig == "" {
		return errors.New("bootstrap secret or static runner config is required")
	}
	paymentRequested := p.KeystoreFile != "" || p.KeystorePasswordFile != "" || p.PaymentDB != "" || p.PaymentRPCURL != nil || p.PaymentChainID != nil || p.PaymentController != nil || p.WeiPerUSD != nil || p.ETHUSDFeed != nil || p.TicketFaceValue != nil || p.TicketWinProb != nil || p.PaymentMaxFeePerGas != nil
	if paymentRequested {
		if p.KeystoreFile == "" || p.KeystorePasswordFile == "" || p.PaymentDB == "" || p.PaymentRPCURL == nil || p.PaymentChainID == nil || p.PaymentController == nil || (p.WeiPerUSD == nil && p.ETHUSDFeed == nil) || p.TicketFaceValue == nil || p.TicketWinProb == nil {
			return errors.New("on-chain payment requires payment-db, keystore-file, keystore-password-file, RPC, chain-id, controller, a fixed rate or ETH/USD feed, face-value and win-prob")
		}
		if *p.PaymentController == (ethcommon.Address{}) {
			return errors.New("payment-controller-address must be nonzero")
		}
		if *p.PaymentChainID == 0 || p.TicketFaceValue.IsZero() || p.TicketWinProb.IsZero() {
			return errors.New("payment chain ID and ticket values must be positive")
		}
		if p.TicketWinProb.Eq(new(uint256.Int).SetAllOne()) {
			return errors.New("ticket-win-prob must be less than 2^256 - 1")
		}
		if p.PaymentMaxFeePerGas != nil && p.PaymentMaxFeePerGas.IsZero() {
			return errors.New("payment-max-fee-per-gas must be positive")
		}
		if p.ETHUSDFeed != nil {
			if p.WeiPerUSD != nil || *p.ETHUSDFeed == (ethcommon.Address{}) || p.PriceMaxAge <= 0 {
				return errors.New("eth-usd-feed requires a nonzero address, positive price-max-age and no fixed wei-per-usd")
			}
		} else if p.WeiPerUSD.Sign() <= 0 {
			return errors.New("wei-per-usd must be positive")
		}
		if err := destination.ValidateURL(p.PaymentRPCURL); err != nil {
			return errors.New("invalid payment RPC URL")
		}
	}
	if err := validateProxyTemplate(p.ProxyURLTemplate); err != nil {
		return err
	}
	if !p.Listen.IsValid() {
		return errors.New("listen must be IP:port")
	}
	if !p.MetricsListen.Addr().IsLoopback() {
		return errors.New("metrics listener must bind loopback")
	}
	for _, endpoint := range []struct {
		name string
		url  *url.URL
	}{{"service-url", p.ServiceURL.Value}, {"runner-service-url", p.RunnerServiceURL.Value}} {
		if endpoint.name == "runner-service-url" && endpoint.url == nil {
			continue
		}
		if u := endpoint.url; destination.ValidateURL(u) != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return fmt.Errorf("%s must be an absolute HTTP or HTTPS URL without credentials, query or fragment", endpoint.name)
		}
	}
	if p.HeartbeatInterval <= 0 || p.HeartbeatTTL <= p.HeartbeatInterval {
		return errors.New("heartbeat-ttl must exceed positive heartbeat-interval")
	}
	for _, item := range []struct {
		purpose string
		grants  []string
	}{{"runner", p.RunnerGrants}, {"session-proxy", p.SessionProxyGrants}, {"static-runner-health", p.HealthGrants}} {
		if _, err := destination.New(item.purpose, item.grants); err != nil {
			return err
		}
	}
	return nil
}

func Root(out, errOut io.Writer) *cobra.Command {
	cmd := (boa.Cmd[Params]{
		Use: "livepeer-orchestrator", Short: "Standalone Live Runner orchestrator", Version: version.String(),
		RejectUnknown: true,
		ConfigFormat:  boa.UniversalConfigFormat(unmarshalConfig),
		PreValidateFunc: func(p *Params, _ *cobra.Command, _ []string) error {
			if p.PrintConfig {
				return nil
			}
			return boa.NewUserInputError(p.Validate())
		},
		ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_ORCHESTRATOR")),
		Args:        cobra.NoArgs,
		RunFuncCtxE: func(ctx *boa.HookContext, p *Params, cmd *cobra.Command, _ []string) error {
			if p.PrintConfig {
				return printConfig(ctx, p, cmd.OutOrStdout())
			}
			return Serve(cmd.Context(), *p, cmd.ErrOrStderr())
		},
	}).ToCobra()
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.AddCommand(redemptionCommand())
	cmd.InitDefaultCompletionCmd()
	return cmd
}

func printConfig(ctx *boa.HookContext, p *Params, out io.Writer) error {
	// Only audited fields are printed; secrets and sensitive paths stay omitted.
	values := map[string]any{
		"Listen": &p.Listen, "MetricsListen": &p.MetricsListen,
		"RunnerGrants": &p.RunnerGrants, "SessionProxyGrants": &p.SessionProxyGrants, "HealthGrants": &p.HealthGrants,
		"HeartbeatInterval": &p.HeartbeatInterval, "HeartbeatTTL": &p.HeartbeatTTL,
		"PaymentDB": &p.PaymentDB, "PaymentChainID": &p.PaymentChainID, "PaymentMaxFeePerGas": &p.PaymentMaxFeePerGas,
		"PaymentController": &p.PaymentController, "WeiPerUSD": &p.WeiPerUSD, "ETHUSDFeed": &p.ETHUSDFeed,
		"PriceMaxAge": &p.PriceMaxAge, "TicketFaceValue": &p.TicketFaceValue, "TicketWinProb": &p.TicketWinProb,
	}
	for name, ptr := range values {
		if !ctx.HasValue(ptr) {
			delete(values, name)
		}
	}
	data, err := toml.Marshal(values)
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
		Runners []StaticRunner
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

func Serve(parent context.Context, p Params, logOut io.Writer) (result error) {
	if err := p.Validate(); err != nil {
		return err
	}
	runnerPolicy, err := destination.New("runner", p.RunnerGrants)
	if err != nil {
		return err
	}
	proxyPolicy, err := destination.New("session-proxy", p.SessionProxyGrants)
	if err != nil {
		return err
	}
	healthPolicy, err := destination.New("static-runner-health", p.HealthGrants)
	if err != nil {
		return err
	}
	registry := NewRegistry(p.BootstrapSecret, p.ServiceURL.String(), p.HeartbeatInterval, p.HeartbeatTTL)
	registry.runnerService = strings.TrimRight(p.RunnerServiceURL.String(), "/")
	var engine *pm.Engine
	var paymentStore *pm.SQLiteStore
	var paymentChain eth.PaymentChain
	var redeemerKey *eth.Key
	var paymentChainID *big.Int
	if p.KeystoreFile != "" {
		registry.SetWeiPerUSD(p.WeiPerUSD)
		redeemerKey, err = eth.OpenKeystoreFile(p.KeystoreFile, p.KeystorePasswordFile)
		if err != nil {
			return err
		}
		paymentStore, err = pm.OpenSQLite(p.PaymentDB)
		if err != nil {
			return err
		}
		defer paymentStore.Close()
		rpc, err := eth.NewRPC(p.PaymentRPCURL, nil)
		if err != nil {
			return err
		}
		defer rpc.Close()
		paymentChainID = new(big.Int).SetUint64(*p.PaymentChainID)
		if err := rpc.CheckChainID(parent, paymentChainID); err != nil {
			return err
		}
		contracts, err := eth.NewContracts(rpc, *p.PaymentController)
		if err != nil {
			return err
		}
		if p.PaymentMaxFeePerGas != nil {
			contracts.MaxFeePerGas = p.PaymentMaxFeePerGas.ToBig()
		}
		paymentChain = eth.PaymentChain{Contracts: contracts}
		if p.ETHUSDFeed != nil {
			rate, until, err := contracts.WeiPerUSD(parent, *p.ETHUSDFeed, p.PriceMaxAge)
			if err != nil {
				return err
			}
			registry.setRate(rate, until)
		}
		engine, err = pm.NewEngine(paymentStore, pm.EthereumChain{Client: paymentChain}, redeemerKey.Address(), p.TicketFaceValue.ToBig(), p.TicketWinProb.ToBig())
		if err != nil {
			return err
		}
	}
	registry.proxyTemplate = p.ProxyURLTemplate
	if err := loadStatic(p.RunnerConfig, registry); err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(logOut, nil))
	app := NewServer(registry, runnerPolicy, proxyPolicy, logger)
	defer app.Close()
	app.SetPayment(engine)
	mainListener, err := net.Listen("tcp", p.Listen.String())
	if err != nil {
		return err
	}
	defer mainListener.Close()
	metricsListener, err := net.Listen("tcp", p.MetricsListen.String())
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
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	mainServer := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: app, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20, MaxHeaderValueCount: 128}
	metricsServer := &http.Server{Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 16, MaxHeaderValueCount: 128}
	errs := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Go(func() { errs <- mainServer.Serve(mainListener) })
	workers.Go(func() { errs <- metricsServer.Serve(metricsListener) })
	logger.Info("orchestrator started", "listen", p.Listen, "metrics_listen", p.MetricsListen)
	// Independent bounded workers keep slow RPC/health calls off the billing
	// and expiry paths. Every worker uses the shutdown context and is joined.
	startWorker := func(period, timeout time.Duration, work func(context.Context)) {
		workers.Go(func() {
			ticker := time.NewTicker(period)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					tickCtx, done := context.WithTimeout(ctx, timeout)
					work(tickCtx)
					done()
				}
			}
		})
	}
	startWorker(p.HeartbeatInterval, time.Second, func(context.Context) { registry.Expire() })
	workers.Go(func() { app.runO2RKeepalives(ctx, 10*time.Second) })
	if engine != nil {
		if p.ETHUSDFeed != nil {
			startWorker(30*time.Second, 30*time.Second, func(ctx context.Context) {
				rate, until, err := paymentChain.Contracts.WeiPerUSD(ctx, *p.ETHUSDFeed, p.PriceMaxAge)
				if err != nil {
					logger.Error("price feed unavailable", "error", err)
					return
				}
				registry.setRate(rate, until)
			})
		}
		startWorker(time.Second, 5*time.Second, app.ChargePaidSessions)
		startWorker(time.Minute, 5*time.Second, func(ctx context.Context) {
			if err := engine.PruneControlState(ctx); err != nil {
				logger.Error("payment retention", "error", err)
			}
		})
		startWorker(p.HeartbeatInterval, 30*time.Second, func(ctx context.Context) {
			for _, err := range pm.ProcessRedemptions(ctx, paymentStore, paymentChain, redeemerKey, paymentChainID) {
				logger.Error("payment redemption", "error", err)
			}
		})
	}
	client := healthPolicy.Client()
	client.Timeout = 5 * time.Second
	defer client.CloseIdleConnections()
	if p.RunnerConfig != "" {
		startWorker(p.HeartbeatInterval, 30*time.Second, func(ctx context.Context) { registry.CheckStaticHealth(ctx, client) })
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	}
	cancel()
	app.Close()
	shutdownCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	if err := mainServer.Shutdown(shutdownCtx); err != nil {
		result = errors.Join(result, err)
		_ = mainServer.Close()
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		result = errors.Join(result, err)
		_ = metricsServer.Close()
	}
	workers.Wait()
	return result
}
