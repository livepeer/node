package chain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/BurntSushi/toml"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

const testSender = "0x0123456789abcdef0123456789abcdef01234567"

func TestLeafHelpAndCompletion(t *testing.T) {
	t.Setenv("LIVEPEER_CHAIN_KEYSTORE_FILE", "/missing/account.json")
	t.Setenv("LIVEPEER_CHAIN_KEYSTORE_PASSWORD_FILE", "/missing/password")
	for _, tc := range []struct {
		command         string
		present, absent []string
	}{
		{"status", []string{"--config", "--output", "--chain-id"}, []string{"--amount", "--reserve", "--submit", "--quiet"}},
		{"stake bond", []string{"--amount", "--submit", "--wait", "--quiet"}, []string{"--delegate", "--reserve", "--lock-id"}},
		{"ticketbroker fund", []string{"--amount", "--reserve", "--quiet"}, []string{"--lock-id", "--fee-share"}},
	} {
		for _, completion := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/completion=%v", tc.command, completion), func(t *testing.T) {
				var output bytes.Buffer
				root := Root(&output, &output)
				args := append(strings.Fields(tc.command), "--help")
				if completion {
					args = append(append([]string{"__complete"}, strings.Fields(tc.command)...), "--")
				}
				root.SetArgs(args)
				require.NoError(t, root.Execute())
				for _, flag := range tc.present {
					require.Contains(t, output.String(), flag)
				}
				for _, flag := range tc.absent {
					require.NotContains(t, output.String(), flag)
				}
				if !completion && tc.command == "stake bond" {
					require.Contains(t, output.String(), "<orchestrator>")
				}
			})
		}
	}
	root := Root(&bytes.Buffer{}, &bytes.Buffer{})
	root.SetArgs([]string{"completion", "bash"})
	require.NoError(t, root.Execute())
}

func TestActionValidationBeforeRPC(t *testing.T) {
	path, passwordPath := test.WriteKeystore(t, nil)
	require.NoError(t, os.WriteFile(passwordPath, []byte("wrong-secret"), 0600))
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected RPC", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("LIVEPEER_CHAIN_RPC_URL", server.URL)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"stake", "bond", "--amount", "1"}, "arg(s)"},
		{[]string{"stake", "bond", testSender, testSender, "--amount", "1"}, "arg(s)"},
		{[]string{"stake", "bond", "not-an-address", "--amount", "1"}, "invalid"},
		{[]string{"stake", "bond", testSender}, "amount"},
		{[]string{"stake", "bond", "--delegate", testSender, "--amount", "1"}, "unknown flag"},
		{[]string{"stake", "unbond", "--amount", "0"}, "must be positive"},
		{[]string{"stake", "unbond", "--amount", "1", "--reserve", "1"}, "unknown flag"},
		{[]string{"stake", "rebond"}, "lock-id"},
		{[]string{"stake", "withdraw"}, "lock-id"},
		{[]string{"earnings", "claim"}, "end-round"},
		{[]string{"earnings", "withdraw-fees", "--amount", "1"}, "recipient"},
		{[]string{"earnings", "withdraw-fees", "--amount", "0", "--recipient", testSender}, "must be positive"},
		{[]string{"orchestrator", "activate"}, "reward-cut"},
		{[]string{"orchestrator", "activate", "--reward-cut", "0"}, "fee-share"},
		{[]string{"orchestrator", "activate", "--reward-cut", "1000001", "--fee-share", "0"}, "exceeds max"},
		{[]string{"orchestrator", "set-config"}, "is required"},
		{[]string{"orchestrator", "set-config", "--fee-share", "0"}, "required together"},
		{[]string{"orchestrator", "set-config", "--service-uri", "https://user:pass@example.com"}, "absolute HTTP"},
		{[]string{"orchestrator", "set-config", "--service-uri", "ftp://example.com"}, "absolute HTTP"},
		{[]string{"orchestrator", "set-config", "--service-uri", "https://example.com?key=secret"}, "absolute HTTP"},
		{[]string{"orchestrator", "set-config", "--service-uri", "https://example.com#fragment"}, "absolute HTTP"},
		{[]string{"ticketbroker", "fund", "--amount", "1"}, "reserve"},
		{[]string{"ticketbroker", "fund", "--amount", "0", "--reserve", "0"}, "cannot both be zero"},
		{[]string{"ticketbroker", "fund", "--amount", "115792089237316195423570985008687907853269984665640564039457584007913129639935", "--reserve", "1"}, "total exceeds uint256"},
		{[]string{"ticketbroker", "unlock", "--wait"}, "wait requires submit"},
		{[]string{"ticketbroker", "unlock", "--submit"}, "keystore-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-file", path}, "keystore-password-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-password-file", passwordPath}, "keystore-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-file", path, "--keystore-password-file", passwordPath}, "cannot decrypt keystore"},
		{[]string{"status", "--submit"}, "unknown flag"},
		{[]string{"account", "--quiet"}, "unknown flag"},
		{[]string{"status", "--chain-id", "0"}, "below min"},
		{[]string{"status", "--controller-address", "0x0000000000000000000000000000000000000000"}, "must be nonzero"},
		{[]string{"status", "--output", "yaml"}, "allowed values"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(append([]string{"--sender", testSender}, tc.args...))
			err := root.Execute()
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), "wrong-secret")
			require.Empty(t, output.String())
			require.Zero(t, requests.Load())
		})
	}
}

func TestSenderRequiredBeforeRPC(t *testing.T) {
	t.Setenv("LIVEPEER_CHAIN_SENDER", "")
	t.Setenv("LIVEPEER_CHAIN_RPC_URL", "http://localhost:1")
	for _, command := range []string{"account", "orchestrator get", "ticketbroker unlock"} {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(strings.Fields(command))
			require.ErrorContains(t, root.Execute(), "sender is required")
			require.Empty(t, output.String())
		})
	}
}

func TestConfigRejectsInvocationInputs(t *testing.T) {
	for _, entry := range []string{
		`Amount = "1"`, `Reserve = "1"`, `Delegate = "` + testSender + `"`, `Recipient = "` + testSender + `"`,
		`Orchestrator = "` + testSender + `"`, `LockID = "0"`, `EndRound = "0"`, `RewardCut = 0`, `FeeShare = 0`,
		`ServiceURI = "https://example.com"`, `Submit = true`, `Wait = true`, `Quiet = true`,
		`Output = "json"`, `PrintConfig = true`,
	} {
		t.Run(entry, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "chain.toml")
			require.NoError(t, os.WriteFile(config, []byte(entry), 0600))
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs([]string{"ticketbroker", "unlock", "--config", config, "--print-config"})
			err := root.Execute()
			require.Error(t, err)
			require.Contains(t, err.Error(), strconv.Quote(strings.Fields(entry)[0]))
			require.Regexp(t, "unknown config field|forbidden by boa", err.Error())
			require.Empty(t, output.String())
		})
	}
}

func TestActionEnvironmentCannotSupplyInputs(t *testing.T) {
	for _, name := range []string{"AMOUNT", "RESERVE", "DELEGATE", "ORCHESTRATOR", "RECIPIENT", "LOCK_ID", "END_ROUND", "REWARD_CUT", "FEE_SHARE", "SERVICE_URI", "OUTPUT"} {
		t.Setenv("LIVEPEER_CHAIN_"+name, "invalid")
	}
	for _, name := range []string{"SUBMIT", "WAIT", "QUIET", "PRINT_CONFIG"} {
		t.Setenv("LIVEPEER_CHAIN_"+name, "true")
	}
	t.Setenv("LIVEPEER_CHAIN_AMOUNT", "5")
	var output bytes.Buffer
	root := Root(&output, &output)
	root.SetArgs([]string{"stake", "unbond"})
	require.ErrorContains(t, root.Execute(), "missing required param 'amount'")
	require.Empty(t, output.String())
	root = Root(&output, &output)
	root.SetArgs([]string{"orchestrator", "set-config", "--print-config"})
	require.NoError(t, root.Execute())
	require.NotEmpty(t, output.String(), "environment quiet must not suppress output")
	root = Root(&output, &output)
	root.SetArgs([]string{"status"})
	require.ErrorContains(t, root.Execute(), "valid RPC URL is required", "environment print-config must not bypass execution")
}

func TestOperatorSourcesAndFlagPlacement(t *testing.T) {
	envSender := "0x1111111111111111111111111111111111111111"
	cliSender := "0x2222222222222222222222222222222222222222"
	var wantSender, chainID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string
			Params []any
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		result := "0x0"
		switch request.Method {
		case "eth_chainId":
			result = chainID
		case "eth_getBalance":
			require.Equal(t, []any{strings.ToLower(wantSender), "latest"}, request.Params)
		case "eth_getTransactionCount":
			require.Equal(t, []any{strings.ToLower(wantSender), "pending"}, request.Params)
		default:
			t.Errorf("unexpected RPC method %s", request.Method)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
	}))
	defer server.Close()
	dir := t.TempDir()
	rpcFile := filepath.Join(dir, "rpc")
	require.NoError(t, os.WriteFile(rpcFile, []byte(server.URL), 0600))
	config := filepath.Join(dir, "chain.toml")
	require.NoError(t, os.WriteFile(config, []byte(fmt.Sprintf("RPCURLFile = %q\nChainID = 3\nSender = %q\nMaxFeePerGas = '3'\nKeystoreFile = '/missing/account.json'\nKeystorePasswordFile = '/missing/password'\n", rpcFile, testSender)), 0600))
	partialConfig := filepath.Join(dir, "partial.toml")
	require.NoError(t, os.WriteFile(partialConfig, []byte(fmt.Sprintf("RPCURLFile = %q\n", rpcFile)), 0600))

	for _, tc := range []struct {
		name       string
		args       []string
		env        bool
		sender, id string
	}{
		{"config only", []string{"--config", config, "account", "--output", "json"}, false, testSender, "0x3"},
		{"CLI before leaf", []string{"--config", config, "--chain-id", "1", "--sender", cliSender, "--max-fee-per-gas", "1", "--output", "json", "account"}, true, cliSender, "0x1"},
		{"CLI after leaf", []string{"account", "--config", config, "--chain-id", "1", "--sender", cliSender, "--max-fee-per-gas", "1", "--output", "json"}, true, cliSender, "0x1"},
		{"env over config", []string{"--config", config, "account", "--output", "json"}, true, envSender, "0x2"},
		{"env only", []string{"account", "--output", "json"}, true, envSender, "0x2"},
		{"CLI over partial config", []string{"account", "--config", partialConfig, "--chain-id", "1", "--sender", cliSender, "--max-fee-per-gas", "1", "--output", "json"}, false, cliSender, "0x1"},
		{"env over partial config", []string{"account", "--config", partialConfig, "--output", "json"}, true, envSender, "0x2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LIVEPEER_CHAIN_CHAIN_ID", "")
			t.Setenv("LIVEPEER_CHAIN_SENDER", "")
			t.Setenv("LIVEPEER_CHAIN_RPC_URL_FILE", "")
			t.Setenv("LIVEPEER_CHAIN_MAX_FEE_PER_GAS", "")
			if tc.env {
				t.Setenv("LIVEPEER_CHAIN_CHAIN_ID", "2")
				t.Setenv("LIVEPEER_CHAIN_SENDER", envSender)
				t.Setenv("LIVEPEER_CHAIN_RPC_URL_FILE", rpcFile)
				t.Setenv("LIVEPEER_CHAIN_MAX_FEE_PER_GAS", "2")
			}
			wantSender, chainID = ethcommon.HexToAddress(tc.sender).Hex(), tc.id
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(tc.args)
			require.NoError(t, root.Execute())
			require.JSONEq(t, fmt.Sprintf(`{"address":%q,"balance_wei":"0","nonce":0}`, wantSender), output.String())
			output.Reset()
			root = Root(&output, &output)
			root.SetArgs(append(append([]string{}, tc.args...), "--print-config"))
			require.NoError(t, root.Execute())
			var printed map[string]any
			require.NoError(t, toml.Unmarshal(output.Bytes(), &printed))
			require.Equal(t, strings.TrimPrefix(tc.id, "0x"), printed["MaxFeePerGas"], "the fee ceiling must respect CLI, environment, and TOML priority")
		})
	}
}
