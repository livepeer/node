package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func testBinaryMigrations(t *testing.T, install string) {
	t.Helper()
	for _, component := range []struct{ name, flag, config string }{
		{"orchestrator", "--redeemer-db", "RedeemerDB = 'custom/state.sqlite'"},
		{"signer", "--kafka-outbox-db", "[Kafka]\nOutboxDB = 'custom/state.sqlite'"},
	} {
		t.Run(component.name+" migrations", func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, component.name), 0700))
			require.NoError(t, os.WriteFile(filepath.Join(root, component.name, "config.toml"), []byte("KeystoreFile = 'missing-key'\nKeystorePasswordFile = 'missing-password'\n"+component.config), 0600))
			path := filepath.Join(root, "custom", "state.sqlite")
			direct, err := exec.Command(filepath.Join(install, "livepeer-"+component.name),
				"migrate", "--data-dir", root, "up").CombinedOutput()
			require.NoError(t, err, string(direct))
			require.FileExists(t, path)
			entries, err := os.ReadDir(filepath.Join(root, component.name))
			require.NoError(t, err)
			require.Len(t, entries, 1, "migration must not create the default database")
			t.Setenv("LIVEPEER_"+strings.ToUpper(component.name)+"_"+strings.ToUpper(strings.ReplaceAll(strings.TrimPrefix(component.flag, "--"), "-", "_")), "missing.sqlite")
			forwarded, err := exec.Command(filepath.Join(install, "livepeer"),
				component.name, "migrate", "status", "--data-dir", root, component.flag, path).CombinedOutput()
			require.NoError(t, err, string(forwarded))
			require.Equal(t, string(direct), string(forwarded))
			require.Contains(t, string(forwarded), `"applied": true`)
		})
	}
}
