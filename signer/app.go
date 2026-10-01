package signer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/version"
	"github.com/spf13/cobra"
)

func init() {
	boa.RegisterConfigFormat(".toml", toml.Unmarshal)
}

type Params struct {
	Kafka                  *KafkaConfig         `optional:"true"`
	AuthWebhookHeadersFile string               `name:"auth-webhook-headers-file" secretfor:"AuthWebhookHeaders"`
	AuthWebhook            *url.URL             `name:"auth-webhook" optional:"true" secret:"true"`
	AuthWebhookFile        string               `name:"auth-webhook-file" secretfor:"AuthWebhook"`
	AuthWebhookHeaders     Headers              `name:"auth-webhook-headers" optional:"true" secret:"true"`
	MaxTicketEV            *big.Rat             `name:"max-ticket-ev" default:"3000000000000" descr:"Maximum expected ticket value in wei"`
	MaxBatchEV             *big.Rat             `name:"max-batch-ev" default:"20000000000000" descr:"Maximum expected batch value in wei"`
	DepositMultiplier      int64                `name:"deposit-multiplier" default:"1" min:"1" descr:"Maximum face value is deposit divided by this value"`
	MaxHourlyPrice         *big.Rat             `name:"max-hourly-price" required:"true" descr:"Maximum live price in USD per hour"`
	MaxFixedPrice          *big.Rat             `name:"max-fixed-price" required:"true" descr:"Maximum fixed request price in USD"`
	WeiPerUSD              *big.Rat             `name:"wei-per-usd" optional:"true" descr:"Fixed wei per USD conversion, mostly for testing"`
	ETHUSDFeed             ethcommon.Address    `name:"eth-usd-feed" required:"true" default:"0x639Fe6ab55C921f74e7fac1ee960C0B6293ba612" descr:"ETH/USD oracle address (Arbitrum mainnet)"`
	ETHUSDMaxAge           time.Duration        `name:"eth-usd-max-age" default:"2h" descr:"Maximum age of the ETH/USD oracle observation"`
	ConfigFile             string               `name:"config" configfile:"true" file:"true" optional:"true" boa:"noconfig"`
	Listen                 netip.AddrPort       `name:"listen" default:"127.0.0.1:8937"`
	MetricsListen          netip.AddrPort       `name:"metrics-listen" default:"127.0.0.1:8938"`
	RPCURL                 *url.URL             `name:"rpc-url" secret:"true" required:"true"`
	RPCURLFile             string               `name:"rpc-url-file" secretfor:"RPCURL"`
	ChainID                *uint64              `name:"chain-id" min:"1" descr:"Optional expected RPC chain ID"`
	Controller             ethcommon.Address    `name:"controller-address" required:"true" default:"0xD8E8328501E9645d16Cf49539efC04f734606ee4" descr:"Livepeer Controller (Arbitrum mainnet)"`
	KeyFile                string               `name:"private-key-file" file:"true" required:"true"`
	Orchestrators          []boa.Text[*url.URL] `name:"orchestrators" optional:"true"`
}

func (p Params) senderPolicy() (pm.SenderPolicy, error) {
	policy := pm.DefaultSenderPolicy()
	if p.MaxTicketEV != nil {
		policy.MaxTicketEV = p.MaxTicketEV
	}
	if p.MaxBatchEV != nil {
		policy.MaxBatchEV = p.MaxBatchEV
	}
	policy.DepositMultiplier = p.DepositMultiplier
	return policy, policy.Validate()
}

func (p Params) discoveryURLs() []*url.URL {
	urls := make([]*url.URL, len(p.Orchestrators))
	for i, item := range p.Orchestrators {
		urls[i] = item.Value
	}
	return urls
}

func (p Params) Validate() error {
	if p.Kafka != nil {
		if err := p.Kafka.Validate(); err != nil {
			return err
		}
	}
	if p.AuthWebhook != nil || len(p.AuthWebhookHeaders) > 0 {
		if err := validateAuthWebhook(p.AuthWebhook); err != nil {
			return err
		}
	}
	if _, err := p.AuthWebhookHeaders.MarshalText(); err != nil {
		return err
	}
	if _, err := p.senderPolicy(); err != nil {
		return err
	}
	if !p.MetricsListen.Addr().IsLoopback() {
		return errors.New("metrics listener must bind loopback")
	}
	if err := destination.ValidateURL(p.RPCURL); err != nil {
		return errors.New("valid RPC URL is required")
	}
	// A required, parseable address can still be the all-zero address.
	if p.Controller == (ethcommon.Address{}) || p.ETHUSDFeed == (ethcommon.Address{}) {
		return errors.New("controller-address and eth-usd-feed must be nonzero")
	}
	if _, err := newPricePolicy(p.MaxHourlyPrice, p.MaxFixedPrice); err != nil {
		return err
	}
	if p.WeiPerUSD != nil {
		if p.WeiPerUSD.Sign() <= 0 {
			return errors.New("wei-per-usd must be positive")
		}
	} else if p.ETHUSDMaxAge <= 0 {
		return errors.New("eth-usd-feed requires positive eth-usd-max-age")
	}
	for _, endpoint := range p.discoveryURLs() {
		if err := validateDiscoveryURL(endpoint); err != nil {
			return err
		}
	}
	return nil
}

func Serve(p Params) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return serve(ctx, p)
}

func serve(parent context.Context, p Params) error {
	if err := p.Validate(); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(parent)
	defer stop()
	key, err := eth.OpenKeyFile(p.KeyFile)
	if err != nil {
		return err
	}
	service := newService(key)
	defer service.Close()
	service.senderPolicy, _ = p.senderPolicy()
	rpc, err := eth.NewRPC(p.RPCURL, nil)
	if err != nil {
		return err
	}
	defer rpc.Close()
	var chainID *big.Int
	if p.ChainID != nil {
		chainID = new(big.Int).SetUint64(*p.ChainID)
	}
	if err := rpc.CheckChainID(ctx, chainID); err != nil {
		return err
	}
	contracts, err := eth.NewContracts(rpc, p.Controller)
	if err != nil {
		return err
	}
	service.SetPaymentChain(eth.PaymentChain{Contracts: contracts})
	policy, _ := newPricePolicy(p.MaxHourlyPrice, p.MaxFixedPrice)
	if p.WeiPerUSD == nil {
		rate, until, err := contracts.WeiPerUSD(ctx, p.ETHUSDFeed, p.ETHUSDMaxAge)
		if err != nil {
			slog.Error("signer price feed unavailable at startup", "error", err)
		} else if err := policy.setRate(rate, until); err != nil {
			slog.Error("signer price feed invalid at startup", "error", err)
		}
	} else if err := policy.setRate(p.WeiPerUSD, time.Time{}); err != nil {
		return err
	}
	service.pricePolicy = policy
	if p.AuthWebhook != nil {
		if err := service.SetAuthWebhook(p.AuthWebhook, p.AuthWebhookHeaders); err != nil {
			return err
		}
	}
	if err := service.SetDiscovery(p.discoveryURLs()); err != nil {
		return err
	}
	producer, err := openKafkaProducer(ctx, p.Kafka, key.Address().Hex())
	if err != nil {
		return err
	}
	defer func() { _ = producer.Close() }()
	if producer != nil {
		service.events = producer
	}
	listener, err := net.Listen("tcp", p.Listen.String())
	if err != nil {
		return err
	}
	defer listener.Close()
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
	metricsMux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		funds, err := service.paymentChain.SenderInfo(ctx, key.Address(), ethcommon.Address{})
		if err != nil || pm.ValidateSenderFunds(funds) != nil || !service.pricePolicy.ready() || !producer.ready(ctx) {
			http.Error(w, "signer payment dependencies unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ready\n")
	})
	metricsMux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.WriteString(w, "livepeer_signer_up 1\n")
		producer.metrics(r.Context(), w)
	})
	srv := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: service, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, MaxHeaderValueCount: 128}
	metricsServer := &http.Server{BaseContext: srv.BaseContext, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, MaxHeaderValueCount: 128}
	errCh := make(chan error, 2)
	var workers sync.WaitGroup
	// Keep delivery alive until in-flight HTTP handlers have finished enqueuing.
	publisherCtx, publisherStop := context.WithCancel(context.Background())
	defer publisherStop()
	if producer != nil {
		workers.Go(func() { producer.run(publisherCtx) })
	}
	if p.WeiPerUSD == nil {
		workers.Go(func() {
			refreshPriceFeed(ctx, 30*time.Second, service.pricePolicy, func(readCtx context.Context) (*big.Rat, time.Time, error) {
				return contracts.WeiPerUSD(readCtx, p.ETHUSDFeed, p.ETHUSDMaxAge)
			})
		})
	}
	workers.Go(func() { errCh <- srv.Serve(listener) })
	workers.Go(func() { errCh <- metricsServer.Serve(metricsListener) })
	select {
	case <-ctx.Done():
	case err = <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutdownErr := srv.Shutdown(shutdown); shutdownErr != nil {
		err = errors.Join(err, shutdownErr)
		_ = srv.Close()
	}
	if shutdownErr := metricsServer.Shutdown(shutdown); shutdownErr != nil {
		err = errors.Join(err, shutdownErr)
		_ = metricsServer.Close()
	}
	publisherStop()
	workers.Wait()
	return err
}

func Root(out, errOut io.Writer) *cobra.Command {
	cmd := (boa.Cmd[Params]{
		Use: "livepeer-signer", Short: "Standalone remote signer", Version: version.String(),
		RejectUnknown: true,
		ParamEnrich:   boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_SIGNER")),
		Args:          cobra.NoArgs,
		RunFuncE: func(p *Params, _ *cobra.Command, _ []string) error {
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
