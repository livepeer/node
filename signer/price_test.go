package signer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm/wire"
	"github.com/stretchr/testify/require"
)

func TestPricePolicyExactCeilingsAndRateExpiry(t *testing.T) {
	p, err := newPricePolicy("1/3", "1/2")
	require.NoError(t, err)
	require.False(t, p.ready())
	require.Equal(t, 503, p.check("live", wire.PriceInfo{PricePerUnit: 1, UnitsPerPrice: 1}).(paymentFailure).status)
	require.NoError(t, p.setRate(big.NewRat(3, 1), time.Now().Add(time.Minute)))
	require.True(t, p.ready())
	require.NoError(t, p.check("live", wire.PriceInfo{PricePerUnit: 1, UnitsPerPrice: 1}))
	require.Equal(t, 481, p.check("live", wire.PriceInfo{PricePerUnit: 4, UnitsPerPrice: 3}).(paymentFailure).status)
	require.NoError(t, p.check("fixed", wire.PriceInfo{PricePerUnit: 3, UnitsPerPrice: 2}))
	require.Equal(t, 481, p.check("fixed", wire.PriceInfo{PricePerUnit: 5, UnitsPerPrice: 3}).(paymentFailure).status)
	require.NoError(t, p.setRate(big.NewRat(6, 1), time.Now().Add(time.Minute)))
	require.NoError(t, p.check("live", wire.PriceInfo{PricePerUnit: 2, UnitsPerPrice: 1}), "refreshed rate must take effect")
	p.mu.Lock()
	p.validUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	require.False(t, p.ready())
	require.Equal(t, 503, p.check("fixed", wire.PriceInfo{PricePerUnit: 1, UnitsPerPrice: 1}).(paymentFailure).status)
	require.Error(t, p.setRate(big.NewRat(6, 1), time.Now().Add(-time.Second)))
}

func TestOracleRateEnforcesBothUSDCeilings(t *testing.T) {
	contract, err := abi.JSON(strings.NewReader(`[
		{"name":"description","type":"function","inputs":[],"outputs":[{"type":"string"}]},
		{"name":"decimals","type":"function","inputs":[],"outputs":[{"type":"uint8"}]},
		{"name":"latestRoundData","type":"function","inputs":[],"outputs":[{"type":"uint80"},{"type":"int256"},{"type":"uint256"},{"type":"uint256"},{"type":"uint80"}]}
	]`))
	require.NoError(t, err)
	rpcServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string
			Params []json.RawMessage
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		var result any
		if request.Method == "eth_getBlockByNumber" {
			result = map[string]string{"hash": ethcommon.HexToHash("0x1234").Hex()}
		} else {
			var call struct{ Data string }
			if json.Unmarshal(request.Params[0], &call) != nil {
				w.WriteHeader(400)
				return
			}
			data, decodeErr := hex.DecodeString(strings.TrimPrefix(call.Data, "0x"))
			if decodeErr != nil || len(data) < 4 {
				w.WriteHeader(400)
				return
			}
			method, methodErr := contract.MethodById(data[:4])
			if methodErr != nil {
				w.WriteHeader(400)
				return
			}
			var args []any
			switch method.Name {
			case "description":
				args = []any{"ETH/USD"}
			case "decimals":
				args = []any{uint8(8)}
			case "latestRoundData":
				args = []any{big.NewInt(10), big.NewInt(2000_00000000), big.NewInt(1), big.NewInt(time.Now().Add(-time.Minute).Unix()), big.NewInt(10)}
			}
			packed, packErr := method.Outputs.Pack(args...)
			if packErr != nil {
				w.WriteHeader(500)
				return
			}
			result = "0x" + hex.EncodeToString(packed)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer rpcServer.Close()
	rpc, err := eth.OpenRPC(rpcServer.URL, []string{strings.TrimPrefix(rpcServer.URL, "http://")}, "")
	require.NoError(t, err)
	contracts, err := eth.OpenContracts(rpc, "0x1234567890123456789012345678901234567890")
	require.NoError(t, err)
	rate, until, err := contracts.WeiPerUSD(t.Context(), ethcommon.HexToAddress("0x1234567890123456789012345678901234567890"), 2*time.Hour)
	require.NoError(t, err)
	require.Equal(t, big.NewRat(500_000_000_000_000, 1), rate)
	policy, err := newPricePolicy("1/500000000000000", "3/500000000000000")
	require.NoError(t, err)
	require.NoError(t, policy.setRate(rate, until))
	require.NoError(t, policy.check("live", wire.PriceInfo{PricePerUnit: 1, UnitsPerPrice: 1}))
	require.Equal(t, 481, policy.check("live", wire.PriceInfo{PricePerUnit: 2, UnitsPerPrice: 1}).(paymentFailure).status)
	require.NoError(t, policy.check("fixed", wire.PriceInfo{PricePerUnit: 3, UnitsPerPrice: 1}))
	require.Equal(t, 481, policy.check("fixed", wire.PriceInfo{PricePerUnit: 4, UnitsPerPrice: 1}).(paymentFailure).status)
}

func TestPaidSignerRequiresUSDPriceSettings(t *testing.T) {
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	p := Params{Listen: "127.0.0.1:0", MetricsListen: "127.0.0.1:0", KeyFile: keyFile, RPCURL: "https://example.com", ChainID: "1", Controller: "0x1234567890123456789012345678901234567890", PriceMaxAge: 2 * time.Hour}
	require.ErrorContains(t, p.Validate(), "max-live-price-usd-per-second")
	p.MaxLivePriceUSDPerSecond = "1"
	require.ErrorContains(t, p.Validate(), "max-fixed-price-usd")
	p.MaxFixedPriceUSD = "2"
	require.ErrorContains(t, p.Validate(), "exactly one")
	p.WeiPerUSD = "3"
	require.NoError(t, p.Validate())
	p.ETHUSDFeed = "0x1234567890123456789012345678901234567890"
	require.ErrorContains(t, p.Validate(), "exactly one")
	p.WeiPerUSD = ""
	require.NoError(t, p.Validate())
}

func TestSignerUnavailableFeedThenRefresh(t *testing.T) {
	s, info := testService(t)
	policy, err := newPricePolicy("20", "20")
	require.NoError(t, err)
	s.pricePolicy = policy
	req := map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed"}
	require.Equal(t, 503, postPayment(t, s, req).Code)
	require.False(t, policy.ready())
	var available atomic.Bool
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		refreshPriceFeed(ctx, 5*time.Millisecond, policy, func(context.Context) (*big.Rat, time.Time, error) {
			if !available.Load() {
				return nil, time.Time{}, errors.New("feed unavailable")
			}
			return big.NewRat(1, 1), time.Now().Add(time.Second), nil
		})
	}()
	require.Equal(t, 503, postPayment(t, s, req).Code)
	available.Store(true)
	require.Eventually(t, policy.ready, time.Second, time.Millisecond)
	require.Equal(t, 200, postPayment(t, s, req).Code)
	cancel()
	<-done
	require.NoError(t, policy.setRate(big.NewRat(1, 1), time.Now().Add(10*time.Millisecond)))
	require.Eventually(t, func() bool { return !policy.ready() }, time.Second, time.Millisecond)
	require.Equal(t, 503, postPayment(t, s, req).Code)
}

func TestSignerStartsUnreadyWhenFirstFeedReadFails(t *testing.T) {
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Method string }
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		var result any
		if request.Method == "eth_chainId" {
			result = "0x1"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	defer rpc.Close()
	freePort := func() int {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		return port
	}
	listen := fmt.Sprintf("127.0.0.1:%d", freePort())
	metrics := fmt.Sprintf("127.0.0.1:%d", freePort())
	p := Params{Listen: listen, MetricsListen: metrics, KeyFile: keyFile, RPCURL: rpc.URL, RPCGrants: []string{strings.TrimPrefix(rpc.URL, "http://")}, ChainID: "1", Controller: "0x1234567890123456789012345678901234567890", MaxLivePriceUSDPerSecond: "20", MaxFixedPriceUSD: "20", ETHUSDFeed: "0x1234567890123456789012345678901234567890", PriceMaxAge: 2 * time.Hour}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, p) }()
	client := &http.Client{Timeout: time.Second}
	require.Eventually(t, func() bool {
		response, err := client.Get("http://" + metrics + "/readyz")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 503
	}, 3*time.Second, 10*time.Millisecond)
	_, info := testService(t)
	data, err := json.Marshal(map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed"})
	require.NoError(t, err)
	response, err := client.Post("http://"+listen+"/generate-live-payment", "application/json", bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 503, response.StatusCode)
	require.NoError(t, response.Body.Close())
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("signer did not stop after context cancellation")
	}
}
