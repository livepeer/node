package chain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
	"github.com/stretchr/testify/require"
)

func TestAccountCreate(t *testing.T) {
	const password = "  account creation password\r\n"
	var previous common.Address
	for _, source := range []string{"config password file", "environment password"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			path, passwordPath := filepath.Join(dir, "account.json"), filepath.Join(dir, "password")
			require.NoError(t, os.WriteFile(passwordPath, []byte(password), 0600))
			args := []string{"account", "create"}
			if source == "config password file" {
				config := filepath.Join(dir, "chain.toml")
				require.NoError(t, os.WriteFile(config, []byte(fmt.Sprintf("KeystoreFile = 'account.json'\nKeystorePasswordFile = 'password'\nAccount = %q\n", testAccount)), 0600))
				args = append(args, "--config", config, "--output", "json")
			} else {
				t.Setenv("LIVEPEER_CHAIN_KEYSTORE_PASSWORD", password)
				args = append(args, "--keystore-file", path)
			}
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(append(append([]string{}, args...), "--print-config"))
			require.NoError(t, root.Execute())
			require.NoFileExists(t, path, "printing configuration must not create an account")
			output.Reset()
			root = Root(&output, &output)
			root.SetArgs(args)
			require.NoError(t, root.Execute(), "creation must work without RPC")
			key, err := eth.OpenKeystoreFile(path, passwordPath)
			require.NoError(t, err, "the generated keystore must load with the exact password bytes")
			require.NotEqual(t, common.Address{}, key.Address())
			require.NotEqual(t, previous, key.Address(), "each creation must generate a fresh account")
			previous = key.Address()
			if source == "config password file" {
				require.JSONEq(t, fmt.Sprintf(`{"address":%q}`, key.Address().Hex()), output.String())
			} else {
				require.Equal(t, "Address: "+key.Address().Hex()+"\n", output.String())
			}
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var stored struct {
				Version int
				ID      string
				Address string
				Crypto  keystore.CryptoJSON
			}
			require.NoError(t, json.Unmarshal(data, &stored))
			require.Equal(t, 3, stored.Version)
			_, err = uuid.Parse(stored.ID)
			require.NoError(t, err)
			require.Equal(t, strings.ToLower(strings.TrimPrefix(key.Address().Hex(), "0x")), stored.Address)
			require.Equal(t, "scrypt", stored.Crypto.KDF)
			require.EqualValues(t, keystore.StandardScryptN, stored.Crypto.KDFParams["n"])
			require.EqualValues(t, keystore.StandardScryptP, stored.Crypto.KDFParams["p"])
			contents, err := os.ReadFile(passwordPath)
			require.NoError(t, err)
			require.Equal(t, password, string(contents))
			output.Reset()
			root = Root(&output, &output)
			root.SetArgs(args)
			require.ErrorContains(t, root.Execute(), "keystore file already exists")
			require.Empty(t, output.String())
			unchanged, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, unchanged)
			temporary, err := filepath.Glob(filepath.Join(dir, ".keystore-*"))
			require.NoError(t, err)
			require.Empty(t, temporary)
		})
	}
}

func TestAccountCreateRejectsUnsafeInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, path, passwordPath *string)
		want   string
	}{
		{"missing keystore option", func(_ *testing.T, p, _ *string) { *p = "" }, "are required"},
		{"missing password option", func(_ *testing.T, _, p *string) { *p = "" }, "are required"},
		{"missing password file", func(_ *testing.T, _, p *string) { *p += ".missing" }, "unavailable"},
		{"readable password file", func(t *testing.T, _, p *string) { require.NoError(t, os.Chmod(*p, 0644)) }, "must be owner-only"},
		{"password directory", func(t *testing.T, _, p *string) { *p = t.TempDir() }, "must be a regular file"},
		{"missing parent", func(_ *testing.T, p, _ *string) { *p = filepath.Join(filepath.Dir(*p), "missing", "account.json") }, "cannot be created"},
		{"existing directory", func(t *testing.T, p, _ *string) { require.NoError(t, os.Mkdir(*p, 0700)) }, "already exists"},
		{"password as output", func(_ *testing.T, p, passwordPath *string) { *p = *passwordPath }, "already exists"},
		{"symlink", func(t *testing.T, p, passwordPath *string) { require.NoError(t, os.Symlink(*passwordPath, *p)) }, "already exists"},
		{"dangling symlink", func(t *testing.T, p, _ *string) { require.NoError(t, os.Symlink(*p+".missing", *p)) }, "already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, passwordPath := filepath.Join(dir, "account.json"), filepath.Join(dir, "password")
			const password = "creation-private-secret"
			require.NoError(t, os.WriteFile(passwordPath, []byte(password), 0600))
			tc.change(t, &path, &passwordPath)
			before, beforeErr := os.Lstat(path)
			address, err := eth.CreateKeystore(path, nil, passwordPath)
			require.Equal(t, common.Address{}, address)
			require.ErrorContains(t, err, tc.want)
			for _, secret := range []string{password, path, passwordPath} {
				if secret != "" {
					require.NotContains(t, err.Error(), secret)
				}
			}
			after, afterErr := os.Lstat(path)
			if beforeErr == nil {
				require.NoError(t, afterErr)
				require.True(t, os.SameFile(before, after), "existing paths must remain untouched")
			} else {
				require.ErrorIs(t, afterErr, os.ErrNotExist)
			}
			contents, err := os.ReadFile(filepath.Join(dir, "password"))
			require.NoError(t, err)
			require.Equal(t, password, string(contents))
			entries, err := filepath.Glob(filepath.Join(dir, ".keystore-*"))
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}
