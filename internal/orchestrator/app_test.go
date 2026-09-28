package orchestrator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestConfigPrecedenceAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("service_url = 'https://file.example'\nlisten = '127.0.0.1:8000'\n"), 0600))
	t.Setenv("LIVEPEER_ORCHESTRATOR_SERVICE_URL", "https://env.example")
	t.Setenv("LIVEPEER_ORCHESTRATOR_BOOTSTRAP_SECRET", "super-secret-env")
	output, err := execute(t, "--config", path, "--service-url", "https://flag.example", "--print-config")
	require.NoError(t, err)
	require.Contains(t, output, "https://flag.example")
	require.Contains(t, output, "127.0.0.1:8000")
	require.NotContains(t, output, "env.example")
	require.NotContains(t, output, "super-secret-env")
	require.NotContains(t, output, path)
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
