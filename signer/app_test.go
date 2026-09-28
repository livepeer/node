package signer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSignerConfigRejectsDirectSecretsAndRedactsDiscoveryURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signer.toml")
	require.NoError(t, os.WriteFile(path, []byte("auth_token = 'direct-secret'\n"), 0600))
	var output bytes.Buffer
	command := Root(&output, &output)
	command.SetArgs([]string{"--config", path, "--print-config"})
	require.Error(t, command.Execute())
	require.NoError(t, os.WriteFile(path, []byte("orchestrators = ['https://user:password@example.org']\nlisten = '127.0.0.1:9000'\n"), 0600))
	output.Reset()
	command = Root(&output, &output)
	command.SetArgs([]string{"--config", path, "--print-config"})
	require.NoError(t, command.Execute())
	require.NotContains(t, output.String(), "password")
	require.Contains(t, output.String(), "127.0.0.1:9000")
}
