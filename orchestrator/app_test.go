package orchestrator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestPaidStartupWithDirectTLS(t *testing.T) {
	fixture := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	certificate := fixture.TLS.Certificates[0]
	fixture.Close()
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	require.NoError(t, err)
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600))
	mainPort, metricsPort := freeTCPPort(t), freeTCPPort(t)
	address := fmt.Sprintf("https://127.0.0.1:%d", mainPort)
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
	p.Listen, p.MetricsListen = fmt.Sprintf("127.0.0.1:%d", mainPort), fmt.Sprintf("127.0.0.1:%d", metricsPort)
	p.ServiceURL, p.TLSCertFile, p.TLSKeyFile = address, certPath, keyPath
	require.NoError(t, p.Validate())
	trust := x509.NewCertPool()
	require.True(t, trust.AppendCertsFromPEM(certPEM))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust}}}
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
		response, err := client.Get(address + "/discovery")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
	require.EqualValues(t, 1, requests.Load())
	_, err = os.Stat(p.PaymentDB)
	require.NoError(t, err)
	response, err := http.Get("http://" + p.MetricsListen + "/readyz")
	require.NoError(t, err)
	response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	p.TLSKeyFile = ""
	require.ErrorContains(t, p.Validate(), "configured together")
}

func TestConfigPrecedenceAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("service_url = 'https://file.example'\nlisten = '127.0.0.1:8000'\npayment_max_fee_per_gas = '100'\nkeystore_file = '/missing/account.json'\nkeystore_password_file = '/missing/password'\n"), 0600))
	t.Setenv("LIVEPEER_ORCHESTRATOR_SERVICE_URL", "https://env.example")
	t.Setenv("LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET", "super-secret-env")
	t.Setenv("LIVEPEER_ORCHESTRATOR_LISTEN", "127.0.0.1:8001")
	t.Setenv("LIVEPEER_ORCHESTRATOR_PAYMENT_MAX_FEE_PER_GAS", "200")
	output, err := execute(t, "--config", path, "--listen", "127.0.0.1:8002", "--service-url", "https://user:password@flag.example", "--payment-max-fee-per-gas", "300", "--print-config")
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
	require.Contains(t, output, `payment_max_fee_per_gas = "300"`)
}

func TestConfigRejectsUnknownAndDirectSecrets(t *testing.T) {
	for _, content := range []string{
		"unknown_key = 1\n",
		"bootstrap_secret = 'forbidden'\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
		_, err := execute(t, "--config", path, "--print-config")
		require.Error(t, err, content)
	}
	_, err := execute(t, "--bootstrap-secret", "literal", "--print-config")
	require.Error(t, err)
}

func TestSecretEnvironmentFileConflict(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("file-secret\n"), 0600))
	t.Setenv("LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET", "env-secret")
	_, err := execute(t, "--bootstrap-secret-file", secretFile, "--print-config")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "cannot both be set"), err)
}

func TestSecretFilePreservesExactBytes(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("exact-secret\n"), 0600))
	var resolved string
	cmd := boa.Cmd[Params]{
		Use: "test", RejectUnknown: true,
		ParamEnrich: boa.ParamEnricherCombine(boa.ParamEnricherDefault, boa.ParamEnricherEnv, boa.ParamEnricherEnvPrefix("LIVEPEER_ORCHESTRATOR")),
		RunFuncE:    func(p *Params, _ *cobra.Command, _ []string) error { resolved = p.BootstrapSecret; return nil },
	}
	require.NoError(t, cmd.RunArgsE([]string{"--bootstrap-secret-file", secretFile}))
	require.Equal(t, "exact-secret\n", resolved)
}

func paymentKeystoreParams(t *testing.T, endpoint string) Params {
	t.Helper()
	return Params{
		Listen: "127.0.0.1:0", MetricsListen: "127.0.0.1:0", ServiceURL: "http://127.0.0.1:8935",
		BootstrapSecret: "test", HeartbeatInterval: time.Hour, HeartbeatTTL: 2 * time.Hour,
		PaymentDB: filepath.Join(t.TempDir(), "payments.sqlite"), PaymentRPCURL: endpoint,
		PaymentChainID: "1", PaymentController: "0x0000000000000000000000000000000000001000",
		WeiPerUSD: "1", TicketFaceValue: "1", TicketWinProb: "1",
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
			require.NoError(t, os.WriteFile(config, []byte(fmt.Sprintf("keystore_file = %q\nkeystore_password_file = %q\n", paths[0][0], paths[0][1])), 0600))
			args := []string{"--config", config, "--payment-db", p.PaymentDB, "--payment-chain-id", p.PaymentChainID,
				"--payment-controller-address", p.PaymentController, "--wei-per-usd", "1", "--ticket-face-value", "1", "--ticket-win-prob", "1"}
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
