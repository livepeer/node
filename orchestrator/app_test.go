package orchestrator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/j0sh/boa/pkg/boa"
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

func TestDirectTLSServesWithOperatorCertificate(t *testing.T) {
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
	p := Params{
		Listen: fmt.Sprintf("127.0.0.1:%d", mainPort), MetricsListen: fmt.Sprintf("127.0.0.1:%d", metricsPort),
		ServiceURL: address, BootstrapSecret: "bootstrap", HeartbeatInterval: time.Second,
		HeartbeatTTL: time.Minute, TLSCertFile: certPath, TLSKeyFile: keyPath,
	}
	require.NoError(t, p.Validate())
	trust := x509.NewCertPool()
	require.True(t, trust.AppendCertsFromPEM(certPEM))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: trust}}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, p, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	require.Eventually(t, func() bool {
		response, err := client.Get(address + "/discovery")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
	p.TLSKeyFile = ""
	require.ErrorContains(t, p.Validate(), "configured together")
}

func TestConfigPrecedenceAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("service_url = 'https://file.example'\nlisten = '127.0.0.1:8000'\npayment_max_fee_per_gas = '100'\n"), 0600))
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
