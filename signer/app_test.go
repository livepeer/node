package signer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func validParams(t *testing.T) Params {
	t.Helper()
	return Params{
		Listen:        netip.MustParseAddrPort("0.0.0.0:8937"),
		MetricsListen: netip.MustParseAddrPort("127.0.0.1:8938"),
		KeyFile:       "key", DepositMultiplier: 1,
		RPCURL:         testURL(t, "http://localhost:8545"),
		Controller:     ethcommon.HexToAddress("0x1234567890123456789012345678901234567890"),
		ETHUSDFeed:     ethcommon.HexToAddress("0x639Fe6ab55C921f74e7fac1ee960C0B6293ba612"),
		MaxHourlyPrice: big.NewRat(36, 1), MaxFixedPrice: big.NewRat(1, 1),
		WeiPerUSD:     big.NewRat(100, 1),
		Orchestrators: []boa.Text[*url.URL]{{Value: testURL(t, "http://localhost:8935")}},
	}
}

func TestSignerConfigurationValidation(t *testing.T) {
	require.NoError(t, validParams(t).Validate())
	for _, test := range []struct {
		name   string
		change func(*Params)
		want   string
	}{
		{"empty Kafka", func(p *Params) { p.Kafka = &KafkaConfig{} }, "kafka broker"},
		{"RPC scheme", func(p *Params) { p.RPCURL = testURL(t, "file:///rpc") }, "RPC URL"},
		{"Controller", func(p *Params) { p.Controller = ethcommon.Address{} }, "controller-address"},
		{"feed", func(p *Params) { p.ETHUSDFeed = ethcommon.Address{} }, "eth-usd-feed"},
		{"hourly price", func(p *Params) { p.MaxHourlyPrice = big.NewRat(0, 1) }, "max-hourly-price"},
		{"fixed price", func(p *Params) { p.MaxFixedPrice = big.NewRat(0, 1) }, "max-fixed-price"},
		{"fixed rate", func(p *Params) { p.WeiPerUSD = big.NewRat(0, 1) }, "wei-per-usd"},
		{"feed age", func(p *Params) { p.WeiPerUSD = nil }, "eth-usd-max-age"},
		{"metrics", func(p *Params) { p.MetricsListen = netip.MustParseAddrPort("0.0.0.0:8938") }, "loopback"},
		{"zero deposit multiplier", func(p *Params) { p.DepositMultiplier = 0 }, "deposit multiplier"},
		{"headers without webhook", func(p *Params) { p.AuthWebhookHeaders = Headers{"Authorization": {"Bearer token"}} }, "webhook URL"},
		{"invalid discovery grant", func(p *Params) { p.DiscoveryGrants = []string{"ftp://localhost"} }, "signer-discovery destination grant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := validParams(t)
			test.change(&p)
			require.ErrorContains(t, p.Validate(), test.want)
		})
	}
}

func TestSignerLoadsConfigAndSecretFiles(t *testing.T) {
	folder := t.TempDir()
	rpcFile := filepath.Join(folder, "rpc")
	keyFile := filepath.Join(folder, "key")
	headersFile := filepath.Join(folder, "headers")
	webhookFile := filepath.Join(folder, "webhook")
	require.NoError(t, os.WriteFile(rpcFile, []byte("http://localhost:8545?key=private-token"), 0600))
	require.NoError(t, os.WriteFile(keyFile, []byte("key"), 0600))
	require.NoError(t, os.WriteFile(webhookFile, []byte("http://localhost:9000/auth"), 0600))
	require.NoError(t, os.WriteFile(headersFile, []byte("Authorization: Bearer private-token, X-User: alice"), 0600))
	config := fmt.Sprintf(`Listen = "127.0.0.1:9001"
MetricsListen = "127.0.0.1:9002"
KeyFile = %q
RPCURLFile = %q
AuthWebhookFile = %q
AuthWebhookHeadersFile = %q
ChainID = 42161
ETHUSDMaxAge = "1h"
MaxHourlyPrice = "36"
MaxFixedPrice = "1/2"
MaxTicketEV = "3000000000000"
Orchestrators = ["http://localhost:8935"]
DiscoveryGrants = ["localhost:8935"]
`, keyFile, rpcFile, webhookFile, headersFile)
	configFile := filepath.Join(folder, "signer.toml")
	require.NoError(t, os.WriteFile(configFile, []byte(config), 0600))
	var p Params
	cmd := boa.Cmd[Params]{Params: &p, RawArgs: []string{"--config", configFile}, RejectUnknown: true}
	require.NoError(t, cmd.Validate())
	require.Equal(t, uint64(42161), *p.ChainID)
	require.Equal(t, "0xD8E8328501E9645d16Cf49539efC04f734606ee4", p.Controller.Hex())
	require.Equal(t, "0x639Fe6ab55C921f74e7fac1ee960C0B6293ba612", p.ETHUSDFeed.Hex())
	require.Equal(t, netip.MustParseAddrPort("127.0.0.1:9001"), p.Listen)
	require.Equal(t, keyFile, p.KeyFile)
	require.Equal(t, "private-token", p.RPCURL.Query().Get("key"))
	require.Equal(t, "http://localhost:9000/auth", p.AuthWebhook.String())
	require.Equal(t, "Bearer private-token", http.Header(p.AuthWebhookHeaders).Get("Authorization"))
	require.Equal(t, big.NewRat(36, 1), p.MaxHourlyPrice)
	require.Equal(t, big.NewRat(1, 2), p.MaxFixedPrice)
	require.Equal(t, time.Hour, p.ETHUSDMaxAge)
	require.Equal(t, "http://localhost:8935", p.Orchestrators[0].Value.String())
	require.Equal(t, []string{"localhost:8935"}, p.DiscoveryGrants)
	require.NoError(t, p.Validate())
}

func loadSignerParams(t *testing.T, args []string, config string) (Params, error) {
	t.Helper()
	p := validParams(t)
	p.KeyFile = filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(p.KeyFile, []byte("key"), 0600))
	args = append([]string(nil), args...)
	if config != "" {
		path := filepath.Join(t.TempDir(), "signer.toml")
		require.NoError(t, os.WriteFile(path, []byte(config), 0600))
		args = append(args, "--config", path)
	}
	cmd := boa.Cmd[Params]{Params: &p, RawArgs: args, RejectUnknown: true,
		ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_SIGNER")),
		RunFuncE:    func(p *Params, _ *cobra.Command, _ []string) error { return p.Validate() },
	}
	err := cmd.RunArgsE(args)
	return p, err
}

func TestSignerDiscoveryGrantConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		env    string
		config string
		want   []string
		bad    bool
	}{
		{name: "omitted"},
		{name: "CLI", args: []string{"--discovery-grants", "localhost:8935,[::1]:8935"}, want: []string{"localhost:8935", "[::1]:8935"}},
		{name: "environment", env: "localhost:8935,[::1]:8935", want: []string{"localhost:8935", "[::1]:8935"}},
		{name: "TOML", config: "DiscoveryGrants = ['localhost:8935', '[::1]:8935']\n", want: []string{"localhost:8935", "[::1]:8935"}},
		{name: "default ports", args: []string{"--discovery-grants", "orch.internal,http://localhost"}, want: []string{"orch.internal", "http://localhost"}},
		{name: "invalid CLI", args: []string{"--discovery-grants", "ftp://localhost"}, bad: true},
		{name: "invalid environment", env: "localhost:0", bad: true},
		{name: "invalid TOML", config: "DiscoveryGrants = ['localhost:65536']\n", bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LIVEPEER_SIGNER_DISCOVERY_GRANTS", test.env)
			p, err := loadSignerParams(t, test.args, test.config)
			if test.bad {
				require.ErrorContains(t, err, "signer-discovery destination grant")
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, p.DiscoveryGrants)
		})
	}
}

func TestSignerReadiness(t *testing.T) {
	for _, fixedRate := range []bool{true, false} {
		t.Run(fmt.Sprintf("fixedRate=%v", fixedRate), func(t *testing.T) {
			p := validParams(t)
			if !fixedRate {
				p.WeiPerUSD, p.ETHUSDMaxAge = nil, time.Hour
			}
			key, keyFile := testSignerKey(t)
			p.KeyFile = keyFile
			var failure atomic.Int32
			rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				var result any
				switch req.Method {
				case "eth_chainId":
					result = "0x1"
				case "eth_getBlockByNumber":
					if failure.Load() != 1 {
						result = map[string]string{"number": "0x32", "hash": ethcommon.HexToHash("0x1234").Hex()}
					}
				case "eth_call":
					var call struct {
						Data string `json:"input"`
					}
					require.NoError(t, json.Unmarshal(req.Params[0], &call))
					selector := func(sig string) bool {
						return strings.HasPrefix(call.Data, "0x"+hex.EncodeToString(crypto.Keccak256([]byte(sig))[:4]))
					}
					switch {
					case selector("getContract(bytes32)"):
						result = fmt.Sprintf("0x%064x", 1)
					case selector("lastInitializedRound()"):
						result = fmt.Sprintf("0x%064x", 5)
					case selector("blockHashForRound(uint256)"):
						result = ethcommon.HexToHash("0x1234").Hex()
					case selector("getSenderInfo(address)"):
						deposit, reserve, withdraw := 1000, 1000, 0
						switch failure.Load() {
						case 2:
							deposit = 0
						case 3:
							reserve = 0
						case 4:
							withdraw = 6
						}
						result = fmt.Sprintf("0x%064x%064x%064x%064x", deposit, withdraw, reserve, 0)
					case selector("description()"):
						result = "0x" // The oracle is unavailable during startup.
					default:
						t.Errorf("unexpected RPC call: %s", call.Data)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			}))
			defer rpc.Close()
			p.RPCURL = testURL(t, rpc.URL)
			free := func() netip.AddrPort {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				address := l.Addr().(*net.TCPAddr).AddrPort()
				require.NoError(t, l.Close())
				return address
			}
			p.Listen, p.MetricsListen = free(), free()
			var outbox *eventOutbox
			if fixedRate {
				eventBytes, err := json.Marshal(testEvent())
				require.NoError(t, err)
				p.Kafka = &KafkaConfig{Broker: kafkaBrokerURL(t, "kafka://127.0.0.1:1"), Topic: "signing", OutboxDB: filepath.Join(t.TempDir(), "events.sqlite"), OutboxMaxBytes: int64(len(eventBytes))}
				brokerAddress, err := p.Kafka.brokerAddress()
				require.NoError(t, err)
				outbox, err = openEventOutbox(t.Context(), p.Kafka.OutboxDB, outboxBinding{key.Address().Hex(), brokerAddress, p.Kafka.Topic}, p.Kafka.OutboxMaxBytes)
				require.NoError(t, err)
				defer func() { require.NoError(t, outbox.Close()) }()
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- serve(ctx, p) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(3 * time.Second):
					t.Error("signer did not stop")
				}
			})
			client := &http.Client{Timeout: time.Second}
			get := func(path string) int {
				response, err := client.Get("http://" + p.MetricsListen.String() + path)
				if err != nil {
					return 0
				}
				defer response.Body.Close()
				return response.StatusCode
			}
			if !fixedRate {
				require.Eventually(t, func() bool { return get("/readyz") == 503 }, 3*time.Second, 10*time.Millisecond)
				require.Equal(t, 200, get("/healthz"))
				return
			}
			require.Eventually(t, func() bool { return get("/readyz") == 200 }, 3*time.Second, 10*time.Millisecond)
			for _, state := range []int32{1, 2, 3, 4, 0} {
				failure.Store(state)
				want := 503
				if state == 0 {
					want = 200
				}
				require.Equal(t, want, get("/readyz"))
				require.Equal(t, 200, get("/healthz"))
			}
			_, err := outbox.enqueue(t.Context(), testEvent())
			require.NoError(t, err)
			require.Equal(t, 503, get("/readyz"), "a full outbox must make the signer unready")
			response, err := client.Get("http://" + p.MetricsListen.String() + "/metrics")
			require.NoError(t, err)
			metrics, err := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			require.Contains(t, string(metrics), "livepeer_signer_kafka_pending_events 1")
			pending, err := outbox.pending(t.Context())
			require.NoError(t, err)
			require.NoError(t, outbox.acknowledge(t.Context(), pending))
			require.Equal(t, 200, get("/readyz"), "storage readiness recovers even while Kafka is unavailable")
		})
	}
}
