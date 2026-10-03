package orchestrator

import (
	"bytes"
	"context"
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
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/internal/test"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	cmd := Root(&output, &output)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return output.String(), err
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func TestPaidHTTPStartup(t *testing.T) {
	mainPort, metricsPort := freeTCPPort(t), freeTCPPort(t)
	address := fmt.Sprintf("http://127.0.0.1:%d", mainPort)
	var requests atomic.Int32
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			Method string
			ID     json.RawMessage
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Equal(t, "eth_chainId", req.Method)
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": "0x1"}))
	}))
	defer rpc.Close()
	p := paymentKeystoreParams(t, rpc.URL)
	p.KeystoreFile, p.KeystorePasswordFile = test.WriteKeystore(t, nil)
	p.Listen = netip.MustParseAddrPort(fmt.Sprintf("0.0.0.0:%d", mainPort))
	p.MetricsListen = netip.MustParseAddrPort(fmt.Sprintf("127.0.0.1:%d", metricsPort))
	p.ServiceURL = boa.Text[*url.URL]{Value: mustURL(t, "https://public.example/external")}
	p.RunnerConfig = filepath.Join(t.TempDir(), "runners.toml")
	require.NoError(t, os.WriteFile(p.RunnerConfig, []byte("[[Runners]]\nID = 'r'\nRunnerURL = 'https://runner.example'\nApp = 'test'\n[Runners.PriceInfo]\nPrice = 1\n"), 0600))
	require.NoError(t, p.Validate())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, p, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(3 * time.Second):
			t.Error("orchestrator did not stop")
		}
	})
	require.Eventually(t, func() bool {
		response, err := http.Get(address + "/external/discovery")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
	require.EqualValues(t, 1, requests.Load())
	_, err := os.Stat(p.PaymentDB)
	require.NoError(t, err)
	response, err := http.Get("http://" + p.MetricsListen.String() + "/readyz")
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	response, err = http.Get(address + "/external/discovery")
	require.NoError(t, err)
	defer response.Body.Close()
	var entries []discoveryEntry
	require.NoError(t, json.NewDecoder(response.Body).Decode(&entries))
	require.Len(t, entries, 1)
	require.Equal(t, p.ServiceURL.String(), entries[0].Address)
}

func TestConfigPrecedenceAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`ServiceURL = 'https://file.example'
Listen = '127.0.0.1:8000'
PaymentMaxFeePerGas = '100'
PaymentChainID = 1
PaymentController = '0x0000000000000000000000000000000000000001'
WeiPerUSD = '1/2'
KeystoreFile = '/missing/account.json'
KeystorePasswordFile = '/missing/password'
`), 0600))
	t.Setenv("LIVEPEER_ORCHESTRATOR_SERVICE_URL", "https://env.example")
	t.Setenv("LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET", "super-secret-env")
	t.Setenv("LIVEPEER_ORCHESTRATOR_LISTEN", "127.0.0.1:8001")
	t.Setenv("LIVEPEER_ORCHESTRATOR_PAYMENT_MAX_FEE_PER_GAS", "200")
	t.Setenv("LIVEPEER_ORCHESTRATOR_PAYMENT_CHAIN_ID", "2")
	t.Setenv("LIVEPEER_ORCHESTRATOR_PAYMENT_CONTROLLER_ADDRESS", "0x0000000000000000000000000000000000000002")
	t.Setenv("LIVEPEER_ORCHESTRATOR_WEI_PER_USD", "2/3")
	output, err := execute(t, "--config", path, "--listen", "127.0.0.1:8002",
		"--service-url", "https://user:password@flag.example", "--payment-max-fee-per-gas", "300",
		"--payment-chain-id", "3", "--payment-controller-address", "0x0000000000000000000000000000000000000003", "--print-config")
	require.NoError(t, err)
	require.Contains(t, output, "127.0.0.1:8002")
	require.NotContains(t, output, "127.0.0.1:8001")
	require.NotContains(t, output, "127.0.0.1:8000")
	require.NotContains(t, output, "password")
	require.NotContains(t, output, "keystore")
	require.NotContains(t, output, "/missing/")
	require.NotContains(t, output, "flag.example")
	require.NotContains(t, output, "env.example")
	require.NotContains(t, output, "super-secret-env")
	require.NotContains(t, output, path)
	require.Contains(t, output, `PaymentMaxFeePerGas = "300"`)
	var printed map[string]any
	require.NoError(t, toml.Unmarshal([]byte(output), &printed))
	require.EqualValues(t, 3, printed["PaymentChainID"])
	require.Equal(t, "0x0000000000000000000000000000000000000003", printed["PaymentController"])
	require.Equal(t, "2/3", printed["WeiPerUSD"])
	require.Equal(t, "5s", printed["HeartbeatInterval"])
	for _, key := range []string{"ServiceURL", "PaymentRPCURL", "BootstrapSecret", "KeystoreFile", "KeystorePasswordFile", "RunnerConfig", "PaymentDB", "TicketFaceValue"} {
		require.NotContains(t, printed, key)
	}
}

func TestConfigRejectsUnknownAndDirectSecrets(t *testing.T) {
	for _, content := range []string{
		"unknown_key = 1\n",
		"BootstrapSecret = 'forbidden'\n",
		"PaymentRPCURL = 'https://forbidden.example'\n",
		"payment_chain_id = 1\n",
		"PrintConfig = true\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
		_, err := execute(t, "--config", path, "--print-config")
		require.Error(t, err, content)
	}
	_, err := execute(t, "--bootstrap-secret", "literal", "--print-config")
	require.Error(t, err)
	t.Setenv("LIVEPEER_ORCHESTRATOR_PRINT_CONFIG", "true")
	_, err = execute(t)
	require.ErrorContains(t, err, "bootstrap secret or static runner config")
}

func TestSecretEnvironmentFileConflict(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("file-secret\n"), 0600))
	t.Setenv("LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET", "env-secret")
	_, err := execute(t, "--bootstrap-secret-file", secretFile, "--print-config")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "cannot both be set"), err)
}

func TestSecretFilesPreserveBytesAndRedactErrors(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("exact-secret\n"), 0600))
	var resolved string
	cmd := boa.Cmd[Params]{
		Use: "test", RejectUnknown: true,
		RunFuncE: func(p *Params, _ *cobra.Command, _ []string) error { resolved = p.BootstrapSecret; return nil },
	}
	require.NoError(t, cmd.RunArgsE([]string{"--bootstrap-secret-file", secretFile}))
	require.Equal(t, "exact-secret\n", resolved)
	secret := "https://user:private-password@rpc.example/\n"
	require.NoError(t, os.WriteFile(secretFile, []byte(secret), 0600))
	_, err := execute(t, "--payment-rpc-url-file", secretFile, "--print-config")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private-password")
	require.NotContains(t, err.Error(), "rpc.example")
}

func TestValidationBeforeStartup(t *testing.T) {
	t.Setenv("LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET", "test")
	db := filepath.Join(t.TempDir(), "payments.sqlite")
	_, err := execute(t, "--payment-db", db)
	require.ErrorContains(t, err, "on-chain payment requires")
	require.True(t, boa.IsUserInputError(err))
	_, err = os.Stat(db)
	require.True(t, os.IsNotExist(err))
	_, err = execute(t, "--metrics-listen", "0.0.0.0:8936")
	require.ErrorContains(t, err, "loopback")
	require.True(t, boa.IsUserInputError(err))

	p := paymentKeystoreParams(t, "http://127.0.0.1:1")
	p.KeystoreFile, p.KeystorePasswordFile = "/missing/key", "/missing/password"
	p.TicketWinProb = new(uint256.Int).SetAllOne()
	require.ErrorContains(t, Serve(t.Context(), p, io.Discard), "ticket-win-prob must be less than")
}

func TestStaticRunnerTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runners.toml")
	config := `[[Runners]]
ID = "r"
RunnerURL = "https://runner.example/base"
App = "test"
HealthCode = 204
[Runners.GPU]
ID = "0"
Name = "H100"
VRAMMB = 80000
[Runners.PriceInfo]
Price = 1.5
Currency = "usd"
Unit = "fixed"
`
	require.NoError(t, os.WriteFile(path, []byte(config), 0600))
	registry := NewRegistry("", "https://public.example", time.Second, time.Minute)
	registry.SetWeiPerUSD(big.NewRat(1, 1))
	require.NoError(t, loadStatic(path, registry))
	runner := registry.runners["r"]
	require.Equal(t, 204, runner.HealthCode)
	require.Equal(t, "1.5", runner.USDQuote.Price.String())
	require.Equal(t, &runnerGPU{ID: "0", Name: "H100", VRAMMB: 80000}, runner.GPU)
	for _, price := range []string{"", "Price = 0\n", "Price = 0.0\n"} {
		require.NoError(t, os.WriteFile(path, []byte(strings.Replace(config, "Price = 1.5\n", price, 1)), 0600))
		require.ErrorContains(t, loadStatic(path, registry), "positive")
	}
	require.NoError(t, os.WriteFile(path, []byte(config+"PriceUSD = 2\n"), 0600))
	require.ErrorContains(t, loadStatic(path, registry), "unknown static runner keys")
}

func TestRedemptionValidationBeforeResources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payments.sqlite")
	require.NoError(t, os.WriteFile(path, nil, 0600))
	t.Setenv("LIVEPEER_ORCHESTRATOR_PAYMENT_DB", path)
	t.Setenv("LIVEPEER_ORCHESTRATOR_RPC_URL", "http://127.0.0.1:1")
	t.Setenv("LIVEPEER_ORCHESTRATOR_CHAIN_ID", "1")
	t.Setenv("LIVEPEER_ORCHESTRATOR_SUBMIT", "true")
	hash := ethcommon.Hash{31: 1}.Hex()
	_, err := execute(t, "redemptions", "--retry-transaction", hash)
	require.ErrorContains(t, err, "retry requires --submit")
	require.True(t, boa.IsUserInputError(err))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Empty(t, data, "validation must precede SQLite initialization")
	output, err := execute(t, "redemptions")
	require.NoError(t, err, "environment variables configure inspection, but cannot authorize submission")
	require.JSONEq(t, "[]", output)
}

func paymentKeystoreParams(t *testing.T, endpoint string) Params {
	t.Helper()
	return Params{
		Listen: netip.MustParseAddrPort("127.0.0.1:0"), MetricsListen: netip.MustParseAddrPort("127.0.0.1:0"), ServiceURL: boa.Text[*url.URL]{Value: mustURL(t, "http://127.0.0.1:8935")},
		BootstrapSecret: "test", HeartbeatInterval: time.Hour, HeartbeatTTL: 2 * time.Hour,
		PaymentDB: filepath.Join(t.TempDir(), "payments.sqlite"), PaymentRPCURL: mustURL(t, endpoint),
		PaymentChainID: new(uint64(1)), PaymentController: new(ethcommon.HexToAddress("0x0000000000000000000000000000000000001000")),
		WeiPerUSD: big.NewRat(1, 1), TicketFaceValue: uint256.NewInt(1), TicketWinProb: uint256.NewInt(1),
	}
}

func TestPaymentKeystoreFailuresBeforeStartup(t *testing.T) {
	path, passwordPath := test.WriteKeystore(t, nil)
	require.NoError(t, os.WriteFile(passwordPath, []byte("wrong-secret"), 0600))
	var requests atomic.Int32
	rpc := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer rpc.Close()
	t.Setenv("LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET", "test")
	t.Setenv("LIVEPEER_ORCHESTRATOR_PAYMENT_RPC_URL", rpc.URL)
	for source, name := range []string{"config", "env", "cli"} {
		t.Run(name, func(t *testing.T) {
			p := paymentKeystoreParams(t, rpc.URL)
			paths := [3][2]string{{"/missing/key", "/missing/password"}, {"/missing/key", "/missing/password"}, {"/missing/key", "/missing/password"}}
			paths[source] = [2]string{path, passwordPath}
			config := filepath.Join(t.TempDir(), "orchestrator.toml")
			require.NoError(t, os.WriteFile(config, []byte(fmt.Sprintf("KeystoreFile = %q\nKeystorePasswordFile = %q\n", paths[0][0], paths[0][1])), 0600))
			args := []string{"--config", config, "--payment-db", p.PaymentDB, "--payment-chain-id", strconv.FormatUint(*p.PaymentChainID, 10),
				"--payment-controller-address", p.PaymentController.Hex(), "--wei-per-usd", "1", "--ticket-face-value", "1", "--ticket-win-prob", "1"}
			if source > 0 {
				t.Setenv("LIVEPEER_ORCHESTRATOR_KEYSTORE_FILE", paths[1][0])
				t.Setenv("LIVEPEER_ORCHESTRATOR_KEYSTORE_PASSWORD_FILE", paths[1][1])
			}
			if source > 1 {
				args = append(args, "--keystore-file", paths[2][0], "--keystore-password-file", paths[2][1])
			}
			_, err := execute(t, args...)
			require.ErrorContains(t, err, "cannot decrypt keystore")
			require.NotContains(t, err.Error(), "wrong-secret")
			require.Zero(t, requests.Load())
			_, err = os.Stat(p.PaymentDB)
			require.True(t, os.IsNotExist(err))
		})
	}
	for _, paths := range [][2]string{{}, {path, ""}, {"", passwordPath}} {
		p := paymentKeystoreParams(t, rpc.URL)
		p.KeystoreFile, p.KeystorePasswordFile = paths[0], paths[1]
		err := Serve(t.Context(), p, io.Discard)
		require.ErrorContains(t, err, "on-chain payment requires")
		require.NotContains(t, err.Error(), "wrong-secret")
		require.Zero(t, requests.Load())
		_, err = os.Stat(p.PaymentDB)
		require.True(t, os.IsNotExist(err))
		if paths[0] != "" || paths[1] != "" {
			// Either path alone requests paid mode; never silently start free.
			p = Params{BootstrapSecret: "test", KeystoreFile: paths[0], KeystorePasswordFile: paths[1]}
			require.ErrorContains(t, p.Validate(), "on-chain payment requires")
		}
	}
}
