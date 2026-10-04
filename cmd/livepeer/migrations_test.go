package main

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func testBinaryMigrations(t *testing.T, install string) {
	t.Helper()
	for _, component := range []struct{ name, flag string }{
		{"orchestrator", "--redeemer-db"},
		{"signer", "--kafka-outbox-db"},
	} {
		t.Run(component.name+" migrations", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.sqlite")
			direct, err := exec.Command(filepath.Join(install, "livepeer-"+component.name),
				"migrate", component.flag, path, "up").CombinedOutput()
			require.NoError(t, err, string(direct))
			forwarded, err := exec.Command(filepath.Join(install, "livepeer"),
				component.name, "migrate", "status", component.flag, path).CombinedOutput()
			require.NoError(t, err, string(forwarded))
			require.Equal(t, string(direct), string(forwarded))
			require.Contains(t, string(forwarded), `"applied": true`)
		})
	}
}
