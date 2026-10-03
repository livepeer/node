package chain

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

func TestLocalSigning(t *testing.T) {
	keyPath, passwordPath := test.WriteFixedKeystore(t)
	key, err := eth.OpenKeystoreFile(keyPath, passwordPath)
	require.NoError(t, err)
	const typed = `{"types":{"EIP712Domain":[{"name":"name","type":"string"},{"name":"version","type":"string"},{"name":"chainId","type":"uint256"},{"name":"verifyingContract","type":"address"}],"Person":[{"name":"name","type":"string"},{"name":"wallet","type":"address"}],"Mail":[{"name":"from","type":"Person"},{"name":"to","type":"Person"},{"name":"contents","type":"string"}]},"primaryType":"Mail","domain":{"name":"Ether Mail","version":"1","chainId":1,"verifyingContract":"0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"},"message":{"from":{"name":"Cow","wallet":"0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826"},"to":{"name":"Bob","wallet":"0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB"},"contents":"Hello, Bob!"}}`
	for _, tc := range []struct {
		name         string
		data         []byte
		typed        bool
		expectedHash string
	}{
		{name: "message preserves bytes", data: []byte{'a', '\n', 0, 255, ' '}, expectedHash: ""},
		{name: "EIP712 reference vector", data: []byte(typed), typed: true, expectedHash: "0xbe609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input")
			require.NoError(t, os.WriteFile(path, tc.data, 0600))
			var output bytes.Buffer
			root := Root(&output, &output)
			command, flag := "message", "--message-file"
			if tc.typed {
				command, flag = "typed-data", "--data-file"
			}
			root.SetArgs([]string{"sign", command, flag, path, "--keystore-file", keyPath, "--keystore-password-file", passwordPath, "--account", key.Address().Hex(), "--output", "json"})
			require.NoError(t, root.Execute(), "signing must work without RPC")
			object := decodeObject(t, output.String())
			sig, err := hexutil.Decode(object["signature"].(string))
			require.NoError(t, err)
			sig[64] -= 27
			hash, err := hexutil.Decode(object["hash"].(string))
			require.NoError(t, err)
			if tc.typed {
				require.Equal(t, tc.expectedHash, object["hash"])
			} else {
				require.Equal(t, accounts.TextHash(tc.data), hash)
			}
			public, err := crypto.SigToPub(hash, sig)
			require.NoError(t, err)
			require.Equal(t, key.Address(), crypto.PubkeyToAddress(*public))
		})
	}
	for _, data := range []string{`null`, `{}`, `{"types":{},"primaryType":"Unknown"}`, typed + ` {}`, strings.Replace(typed, `"primaryType":"Mail"`, `"primaryType":"Missing"`, 1), strings.Replace(typed, `"domain":{"name":"Ether Mail","version":"1","chainId":1,"verifyingContract":"0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"}`, `"domain":null`, 1), strings.Replace(typed, `"Hello, Bob!"`, `100`, 1)} {
		path := filepath.Join(t.TempDir(), "input")
		require.NoError(t, os.WriteFile(path, []byte(data), 0600))
		var out bytes.Buffer
		require.Error(t, signFile(OperatorParams{KeystoreFile: keyPath, KeystorePasswordFile: passwordPath}, DisplayOptions{Output: "json"}, &out, path, true))
		require.Empty(t, out.String())
	}
	path := filepath.Join(t.TempDir(), "message")
	require.NoError(t, os.WriteFile(path, []byte("hi"), 0600))
	var out bytes.Buffer
	wrongKey, err := crypto.HexToECDSA(strings.Repeat("0", 63) + "2")
	require.NoError(t, err)
	wrong := crypto.PubkeyToAddress(wrongKey.PublicKey)
	require.ErrorContains(t, signFile(OperatorParams{KeystoreFile: keyPath, KeystorePasswordFile: passwordPath, Account: &wrong}, DisplayOptions{}, &out, path, false), "configured account address does not match")
	// JSON integer literals above 2^53 must retain their exact value.
	first := `{"types":{"EIP712Domain":[{"name":"name","type":"string"}],"Value":[{"name":"n","type":"uint256"}]},"primaryType":"Value","domain":{"name":"Numbers"},"message":{"n":9007199254740993}}`
	hashes := []string{}
	for _, data := range []string{first, strings.Replace(first, "9007199254740993", `"9007199254740993"`, 1)} {
		require.NoError(t, os.WriteFile(path, []byte(data), 0600))
		out.Reset()
		require.NoError(t, signFile(OperatorParams{KeystoreFile: keyPath, KeystorePasswordFile: passwordPath}, DisplayOptions{Output: "json"}, &out, path, true))
		var result map[string]any
		require.NoError(t, json.Unmarshal(out.Bytes(), &result))
		hashes = append(hashes, result["hash"].(string))
	}
	require.Equal(t, hashes[0], hashes[1])
}

func TestEmptyEIP712Domain(t *testing.T) {
	data := apitypes.TypedData{Types: apitypes.Types{"EIP712Domain": {}, "Value": {{Name: "n", Type: "uint256"}}}, PrimaryType: "Value", Message: map[string]any{"n": big.NewInt(7)}}
	hash, err := typedDataHash(data)
	require.NoError(t, err)
	domain := crypto.Keccak256(crypto.Keccak256([]byte("EIP712Domain()")))
	message := crypto.Keccak256(crypto.Keccak256([]byte("Value(uint256 n)")), common.LeftPadBytes(big.NewInt(7).Bytes(), 32))
	require.Equal(t, crypto.Keccak256([]byte{0x19, 0x01}, domain, message), hash)
}
