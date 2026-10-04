package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
)

// WriteConfig writes a TOML fixture in the format selected by its extension.
func WriteConfig(t testing.TB, path, config string) {
	t.Helper()
	data := []byte(config)
	if filepath.Ext(path) == ".json" {
		var values map[string]any
		require.NoError(t, toml.Unmarshal(data, &values))
		var err error
		data, err = json.Marshal(values)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(path, data, 0600))
}
