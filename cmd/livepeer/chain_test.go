package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/keystore"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestChainBinaryQuietExecution(t *testing.T) {
	if testing.Short() {
		t.Skip("real binary integration test")
	}
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	rootDir := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	install := t.TempDir()
	for _, name := range []string{"livepeer", "livepeer-chain"} {
		build := exec.Command("go", "build", "-mod=readonly", "-o", filepath.Join(install, name), "./cmd/"+name)
		build.Dir = rootDir
		output, err := build.CombinedOutput()
		require.NoError(t, err, string(output))
	}
	key, err := crypto.HexToECDSA(strings.Repeat("0", 63) + "1")
	require.NoError(t, err)
	keyAddress := crypto.PubkeyToAddress(key.PublicKey)
	keyFile, passwordPath := filepath.Join(install, "keystore.json"), filepath.Join(install, "password")
	const password = "binary test keystore password"
	keyJSON, err := keystore.EncryptKey(&keystore.Key{Id: uuid.New(), Address: keyAddress, PrivateKey: key}, password, keystore.LightScryptN, keystore.LightScryptP)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyFile, keyJSON, 0600))
	require.NoError(t, os.WriteFile(passwordPath, []byte(password), 0600))
	for _, tc := range []struct {
		name        string
		flags       []string
		failure     string
		wantSent    int32
		wantReceipt int32
		wantErr     string
	}{
		{name: "quiet dry run"},
		{name: "quiet submission", flags: []string{"--submit"}, wantSent: 1, wantReceipt: 1},
		{name: "simulation error", failure: "simulation", wantErr: "simulation failed"},
		{name: "broadcast error", flags: []string{"--submit"}, failure: "broadcast", wantSent: 1, wantErr: "HTTP 503"},
		{name: "receipt error", flags: []string{"--submit", "--wait"}, failure: "receipt", wantSent: 1, wantReceipt: 1, wantErr: "receipt"},
		{name: "validation error", flags: []string{"--no-wait", "--max-transaction-replacements", "1"}, wantErr: "no-wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent, receipts atomic.Int32
			var hash atomic.Value
			controller := ethcommon.HexToAddress("0xD8E8328501E9645d16Cf49539efC04f734606ee4")
			broker := ethcommon.HexToAddress("0x2000")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Method string
					Params []json.RawMessage
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				var result any = "0x1"
				switch req.Method {
				case "eth_getBlockByNumber":
					result = &types.Header{Number: big.NewInt(1), Difficulty: new(big.Int), BaseFee: big.NewInt(1)}
				case "eth_chainId", "eth_maxPriorityFeePerGas", "eth_getTransactionCount":
				case "eth_call":
					var call struct{ To string }
					require.NoError(t, json.Unmarshal(req.Params[0], &call))
					if ethcommon.HexToAddress(call.To) == controller {
						result = "0x" + hex.EncodeToString(ethcommon.LeftPadBytes(broker.Bytes(), 32))
					} else {
						if tc.failure == "simulation" {
							http.Error(w, "simulation unavailable", http.StatusServiceUnavailable)
							return
						}
						result = "0x"
					}
				case "eth_estimateGas":
					result = "0x5208"
				case "eth_sendRawTransaction":
					sent.Add(1)
					var encoded string
					require.NoError(t, json.Unmarshal(req.Params[0], &encoded))
					raw, err := hex.DecodeString(strings.TrimPrefix(encoded, "0x"))
					require.NoError(t, err)
					var tx types.Transaction
					require.NoError(t, tx.UnmarshalBinary(raw))
					hash.Store(tx.Hash().Hex())
					if tc.failure == "broadcast" {
						http.Error(w, "broadcast unavailable", http.StatusServiceUnavailable)
						return
					}
					result = tx.Hash().Hex()
				case "eth_getTransactionReceipt":
					receipts.Add(1)
					if tc.failure == "receipt" {
						http.Error(w, "receipt unavailable", http.StatusServiceUnavailable)
						return
					}
					result = &types.Receipt{TxHash: ethcommon.HexToHash(hash.Load().(string)), Status: 1, BlockNumber: big.NewInt(16), BlockHash: ethcommon.Hash{31: 1}, Logs: []*types.Log{}}
				default:
					t.Errorf("unexpected RPC %s", req.Method)
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
			}))
			defer server.Close()
			dir := t.TempDir()
			rpcFile := filepath.Join(dir, "rpc")
			require.NoError(t, os.WriteFile(rpcFile, []byte(server.URL), 0600))
			config := filepath.Join(dir, "chain.toml")
			require.NoError(t, os.WriteFile(config, []byte(fmt.Sprintf("RPCURLFile = %q\nAccount = %q\nKeystoreFile = %q\nKeystorePasswordFile = %q\n", rpcFile, keyAddress.Hex(), keyFile, passwordPath)), 0600))
			args := append([]string{"chain", "--config", config, "ticketbroker", "unlock", "--quiet", "--output", "json"}, tc.flags...)
			command := exec.Command(filepath.Join(install, "livepeer"), args...)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			if tc.wantErr == "" {
				require.NoError(t, err, stderr.String())
				require.Empty(t, stderr.String())
			} else {
				var exit *exec.ExitError
				require.ErrorAs(t, err, &exit)
				require.Equal(t, 2, exit.ExitCode())
				require.Contains(t, stderr.String(), tc.wantErr)
				if tc.wantSent > 0 {
					require.Contains(t, stderr.String(), hash.Load().(string))
				}
			}
			require.Empty(t, stdout.String())
			require.Equal(t, tc.wantSent, sent.Load())
			require.Equal(t, tc.wantReceipt, receipts.Load())
		})
	}
}
