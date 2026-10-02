package eth_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

func TestOpenKeystorePreservesPasswordAndSigningIdentity(t *testing.T) {
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	// The shared fixture password includes leading spaces and CRLF.
	path, passwordPath := test.WriteKeystore(t, private)
	require.NoError(t, os.Chmod(path, 0400))
	require.NoError(t, os.Chmod(passwordPath, 0400))
	key, err := eth.OpenKeystoreFile(path, passwordPath)
	require.NoError(t, err)
	require.Equal(t, crypto.PubkeyToAddress(private.PublicKey), key.Address())
	message := []byte("keystore account identity")
	for _, sign := range []func() ([]byte, error){
		func() ([]byte, error) { return key.SignMessage(message) },
		func() ([]byte, error) { return key.SignHash(accounts.TextHash(message)) },
	} {
		sig, err := sign()
		require.NoError(t, err)
		require.Contains(t, []byte{27, 28}, sig[64])
		sig[64] -= 27
		pub, err := crypto.SigToPub(accounts.TextHash(message), sig)
		require.NoError(t, err)
		require.Equal(t, key.Address(), crypto.PubkeyToAddress(*pub))
	}
}

func TestOpenKeystoreFailuresAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, path, passwordPath *string)
		want   string
	}{
		{"missing keystore option", func(_ *testing.T, p, _ *string) { *p = "" }, "are required"},
		{"missing password option", func(_ *testing.T, _, p *string) { *p = "" }, "are required"},
		{"missing keystore file", func(_ *testing.T, p, _ *string) { *p += ".missing" }, "keystore file is unavailable"},
		{"missing password file", func(_ *testing.T, _, p *string) { *p += ".missing" }, "password file is unavailable"},
		{"keystore directory", func(t *testing.T, p, _ *string) { *p = t.TempDir() }, "keystore file must be a regular file"},
		{"password directory", func(t *testing.T, _, p *string) { *p = t.TempDir() }, "password file must be a regular file"},
		{"readable keystore", func(t *testing.T, p, _ *string) { require.NoError(t, os.Chmod(*p, 0644)) }, "keystore file must be owner-only"},
		{"readable password", func(t *testing.T, _, p *string) { require.NoError(t, os.Chmod(*p, 0640)) }, "password file must be owner-only"},
		{"wrong password", func(t *testing.T, _, p *string) { require.NoError(t, os.WriteFile(*p, []byte("private-secret"), 0600)) }, "cannot decrypt keystore"},
		{"password newline", func(t *testing.T, _, p *string) {
			data, err := os.ReadFile(*p)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(*p, append(data, '\n'), 0600))
		}, "cannot decrypt keystore"},
		{"malformed JSON", func(t *testing.T, p, _ *string) {
			require.NoError(t, os.WriteFile(*p, []byte("private-secret{"), 0600))
		}, "cannot decrypt keystore"},
		{"plaintext key", func(t *testing.T, p, _ *string) {
			require.NoError(t, os.WriteFile(*p, []byte(strings.Repeat("0", 63)+"1"), 0600))
		}, "cannot decrypt keystore"},
		{"unsupported version", func(t *testing.T, p, _ *string) { changeKeystore(t, *p, func(v map[string]any) { v["version"] = 99 }) }, "cannot decrypt keystore"},
		{"malformed KDF", func(t *testing.T, p, _ *string) {
			changeKeystore(t, *p, func(v map[string]any) {
				v["crypto"].(map[string]any)["kdfparams"].(map[string]any)["n"] = "private-secret"
			})
		}, "cannot decrypt keystore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, passwordPath := test.WriteKeystore(t, nil)
			tc.change(t, &path, &passwordPath)
			key, err := eth.OpenKeystoreFile(path, passwordPath)
			require.Nil(t, key)
			require.ErrorContains(t, err, tc.want)
			for _, secret := range []string{"private-secret", "test keystore password", path, passwordPath} {
				if secret != "" {
					require.NotContains(t, err.Error(), secret)
				}
			}
		})
	}
}

func changeKeystore(t *testing.T, path string, change func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var value map[string]any
	require.NoError(t, json.Unmarshal(data, &value))
	change(value)
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
}
