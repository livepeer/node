package orchestrator

import (
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
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

func TestRegressionOffchainSingleShotCapacityAndHeaders(t *testing.T) {
	seen := make(chan http.Header, 2)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- r.Header.Clone(); <-release; w.WriteHeader(200) }))
	defer upstream.Close()
	defer close(release)
	reg, s := reviewServer(t, upstream.URL)
	require.NoError(t, reg.AddStatic(StaticRunner{ID: "one", RunnerURL: upstream.URL, App: "one", Mode: "single-shot", Capacity: 1}))
	go s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/apps/one/app", nil))
	header := <-seen
	t.Run("session_headers", func(t *testing.T) {
		require.NotEmpty(t, header.Get("Livepeer-Session-Id"), "off-chain single-shot lost its session control headers")
	})
	done := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "/apps/one/app", nil))
		done <- w.Code
	}()
	select {
	case <-seen:
		t.Error("second request reached capacity=1 upstream while first was still running")
	case code := <-done:
		require.Contains(t, []int{409, 503}, code)
	case <-time.After(time.Second):
		t.Fatal("second request neither rejected nor routed")
	}
}
