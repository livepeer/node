package orchestrator

import (
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/livepeer/node/destination"
	"github.com/stretchr/testify/require"
)

func TestRegressionPaymentConfigurationRequiresKey(t *testing.T) {
	p := Params{Listen: "127.0.0.1:8935", MetricsListen: "127.0.0.1:8936", ServiceURL: "http://127.0.0.1:8935", BootstrapSecret: "test", HeartbeatInterval: time.Second, HeartbeatTTL: time.Minute, PaymentDB: "payment.sqlite", PaymentRPCURL: "https://rpc.example", PaymentChainID: "1", PaymentController: "0x0000000000000000000000000000000000000001", WeiPerUSD: "1", TicketFaceValue: "1", TicketWinProb: "1"}
	require.Error(t, p.Validate(), "without payment-key-file the complete payment configuration silently starts off-chain")
}

func reviewServer(t *testing.T, upstream string) (*Registry, *Server) {
	t.Helper()
	p, err := destination.New("runner", []string{strings.TrimPrefix(upstream, "http://")})
	require.NoError(t, err)
	reg := NewRegistry("bootstrap", "https://orch.example", time.Second, time.Minute)
	return reg, NewServer(reg, p, p, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestRegressionHourlyUSDMatchesAdvertisedUnit(t *testing.T) {
	reg, _ := reviewServer(t, "http://127.0.0.1:1")
	reg.SetWeiPerUSD(big.NewRat(3600, 1))
	price := priceInfo{Price: "3600", Currency: "usd", Unit: "hour"}
	require.NoError(t, reg.normalizePrice(&price))
	require.Equal(t, "seconds", price.Unit)
	require.Equal(t, "1", price.PriceUSD.String(), "$3600/hour must advertise $1/second")
}
