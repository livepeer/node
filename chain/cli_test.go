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
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

const testAccount = "0x0123456789abcdef0123456789abcdef01234567"

func TestLeafHelpAndCompletion(t *testing.T) {
	t.Setenv("LIVEPEER_CHAIN_KEYSTORE_FILE", "/missing/account.json")
	t.Setenv("LIVEPEER_CHAIN_KEYSTORE_PASSWORD_FILE", "/missing/password")
	for _, tc := range []struct {
		command         string
		present, absent []string
	}{
		{"status", []string{"--config", "--output", "--chain-id"}, []string{"--amount", "--reserve", "--submit", "--quiet"}},
		{"account create", []string{"--keystore-file", "--keystore-password-file", "--output"}, []string{"--submit", "--amount"}},
		{"account get", []string{"--account", "--output"}, []string{"--submit", "--amount"}},
		{"stake bond", []string{"--amount", "--submit", "--wait", "--quiet"}, []string{"--delegate", "--reserve", "--lock-id"}},
		{"ticketbroker fund", []string{"--amount", "--reserve", "--quiet"}, []string{"--lock-id", "--fee-cut"}},
		{"orchestrator register", []string{"--amount", "--redelegate", "--lock-id", "--reward-cut", "--fee-cut", "--service-uri"}, []string{"--fee-share", "--recipient"}},
		{"stake locks", []string{"--from-id", "--limit", "--withdrawable", "--locked"}, []string{"--submit", "--amount"}},
		{"governance proposal vote", []string{"--reason", "--gas-limit", "--max-priority-fee-per-gas", "--max-transaction-replacements"}, []string{"--amount", "--lock-id"}},
		{"sign typed-data", []string{"--data-file"}, []string{"--message-file", "--submit"}},
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
		{[]string{"stake", "bond", testAccount, testAccount, "--amount", "1"}, "arg(s)"},
		{[]string{"stake", "bond", "not-an-address", "--amount", "1"}, "invalid"},
		{[]string{"stake", "bond", testAccount}, "amount"},
		{[]string{"stake", "bond", testAccount, "--amount", "1", "--redelegate"}, "exactly one"},
		{[]string{"orchestrator", "register", "--amount", "1", "--lock-id", "1", "--reward-cut", "1", "--fee-cut", "2"}, "mutually exclusive"},
		{[]string{"orchestrator", "register", "--redelegate", "--lock-id", "1", "--reward-cut", "1", "--fee-cut", "2"}, "mutually exclusive"},
		{[]string{"stake", "rebond"}, "unknown command"},
		{[]string{"orchestrator", "activate"}, "unknown command"},
		{[]string{"orchestrator", "reward"}, "unknown command"},
		{[]string{"ticketbroker", "unlock", "--transaction-timeout", "0s"}, "must be positive"},
		{[]string{"ticketbroker", "unlock", "--gas-limit", "0"}, "below min"},
		{[]string{"ticketbroker", "fund", "--amount", "all", "--reserve", "0"}, "not supported"},
		{[]string{"governance", "poll", "vote", testAccount, "maybe"}, "allowed values"},
		{[]string{"stake", "locks", "--limit", "1001"}, "exceeds max"},
		{[]string{"stake", "locks", "--withdrawable", "--locked"}, "mutually exclusive"},

		{[]string{"stake", "bond", "--delegate", testAccount, "--amount", "1"}, "unknown flag"},
		{[]string{"stake", "unbond", "--amount", "0"}, "must be positive"},
		{[]string{"stake", "unbond", "--amount", "1", "--reserve", "1"}, "unknown flag"},
		{[]string{"stake", "cancel-unbond"}, "lock-id"},
		{[]string{"stake", "withdraw"}, "lock-id"},
		{[]string{"earnings", "withdraw-fees", "--amount", "0", "--recipient", testAccount}, "must be positive"},
		{[]string{"orchestrator", "register"}, "reward-cut"},
		{[]string{"orchestrator", "register", "--reward-cut", "0"}, "fee-cut"},
		{[]string{"orchestrator", "register", "--reward-cut", "100.0001", "--fee-cut", "0"}, "between 0 and 100"},
		{[]string{"orchestrator", "set-config"}, "is required"},
		{[]string{"orchestrator", "set-config", "--fee-share", "0"}, "unknown flag"},
		{[]string{"orchestrator", "set-config", "--service-uri", "https://user:pass@example.com"}, "absolute HTTP"},
		{[]string{"orchestrator", "set-config", "--service-uri", "ftp://example.com"}, "absolute HTTP"},
		{[]string{"orchestrator", "set-config", "--service-uri", "https://example.com?key=secret"}, "absolute HTTP"},
		{[]string{"orchestrator", "set-config", "--service-uri", "https://example.com#fragment"}, "absolute HTTP"},
		{[]string{"ticketbroker", "fund", "--amount", "1"}, "reserve"},
		{[]string{"ticketbroker", "fund", "--amount", "0", "--reserve", "0"}, "cannot both be zero"},
		{[]string{"ticketbroker", "fund", "--amount", "115792089237316195423570985008687907853269984665640564039457584007913129639935", "--reserve", "1", "--base-units"}, "total exceeds uint256"},
		{[]string{"ticketbroker", "unlock", "--no-wait", "--max-transaction-replacements", "1"}, "no-wait"},
		{[]string{"ticketbroker", "unlock", "--submit"}, "keystore-password"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-file", path}, "keystore-password-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-password-file", passwordPath}, "keystore-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-file", path, "--keystore-password-file", passwordPath}, "cannot decrypt keystore"},
		{[]string{"status", "--submit"}, "unknown flag"},
		{[]string{"account", "--quiet"}, "unknown flag"},
		{[]string{"account", "create"}, "are required"},
		{[]string{"account", "create", "extra"}, "unknown command"},
		{[]string{"status", "--chain-id", "0"}, "below min"},
		{[]string{"status", "--controller-address", "0x0000000000000000000000000000000000000000"}, "must be nonempty"},
		{[]string{"status", "--output", "yaml"}, "allowed values"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(append([]string{"--account", testAccount}, tc.args...))
			err := root.Execute()
			require.ErrorContains(t, err, tc.want)
			require.NotContains(t, err.Error(), "wrong-secret")
			require.Empty(t, output.String())
			require.Zero(t, requests.Load())
		})
	}
}

func TestAccountRequiredBeforeRPC(t *testing.T) {
	t.Setenv("LIVEPEER_CHAIN_DATA_DIR", t.TempDir())
	t.Setenv("LIVEPEER_CHAIN_ACCOUNT", "")
	t.Setenv("LIVEPEER_CHAIN_RPC_URL", "http://localhost:1")
	for _, command := range []string{"account get", "orchestrator get", "ticketbroker unlock"} {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(strings.Fields(command))
			require.ErrorContains(t, root.Execute(), "keystore directory is unavailable")
			require.Empty(t, output.String())
		})
	}
}

func TestConfigRejectsInvocationInputs(t *testing.T) {
	for _, tc := range []struct {
		command string
		load    func([]byte) error
		entries []string
	}{
		{"ticketbroker fund", loadInvocationConfig[FundParams], []string{`Amount = "1"`, `Reserve = "1"`, `BaseUnits = true`}},
		{"stake bond", loadInvocationConfig[BondParams], []string{`Orchestrator = "` + testAccount + `"`, `Redelegate = true`}},
		{"stake cancel-unbond", loadInvocationConfig[CancelUnbondParams], []string{`Delegate = "` + testAccount + `"`, `LockID = "0"`}},
		{"earnings withdraw-fees", loadInvocationConfig[WithdrawFeesParams], []string{`Recipient = "` + testAccount + `"`}},
		{"earnings claim", loadInvocationConfig[ClaimParams], []string{`EndRound = "0"`}},
		{"orchestrator register", loadInvocationConfig[RegisterParams], []string{`RewardCut = "0"`, `FeeCut = "0"`, `ServiceURI = "https://example.com"`}},
		{"orchestrator list", loadInvocationConfig[ListParams], []string{`Active = true`}},
		{"stake locks", loadInvocationConfig[LocksParams], []string{`FromID = "0"`, `Limit = 10`, `Withdrawable = true`, `Locked = true`}},
		{"governance poll vote", loadInvocationConfig[PollVoteParams], []string{`Address = "` + testAccount + `"`, `Choice = "yes"`}},
		{"governance proposal vote", loadInvocationConfig[ProposalVoteParams], []string{`ID = "1"`, `Choice = "for"`, `Reason = "test"`}},
		{"sign message", loadInvocationConfig[MessageParams], []string{`MessageFile = "message.txt"`}},
		{"sign typed-data", loadInvocationConfig[TypedDataParams], []string{`DataFile = "data.json"`}},
		{"ticketbroker unlock", loadInvocationConfig[TransactionOptions], []string{`GasLimit = 100`, `NoWait = true`, `TransactionTimeout = "3m"`, `MaxPriorityFeePerGas = "1"`, `MaxTransactionReplacements = 1`, `Submit = true`, `Wait = true`, `Quiet = true`}},
		{"status", loadInvocationConfig[DisplayOptions], []string{`Output = "json"`, `PrintConfig = true`}},
	} {
		for _, entry := range tc.entries {
			t.Run(tc.command+"/"+entry, func(t *testing.T) {
				config := filepath.Join(t.TempDir(), "chain.toml")
				require.NoError(t, os.WriteFile(config, []byte(entry), 0600))
				var output bytes.Buffer
				root := Root(&output, &output)
				root.SetArgs(append(strings.Fields(tc.command), "--config", config, "--print-config"))
				err := root.Execute()
				require.Error(t, err)
				require.Contains(t, err.Error(), strconv.Quote(strings.Fields(entry)[0]))
				require.Regexp(t, "unknown config field|forbidden by boa", err.Error())
				// The root file rejects leaf fields as unknown. Check the leaf's
				// own source tags as well, independently of that root guard.
				require.ErrorContains(t, tc.load([]byte(entry)), "forbidden by boa")
				require.Empty(t, output.String())
			})
		}
	}
}

func loadInvocationConfig[T any](data []byte) error {
	return boa.LoadConfigBytes(data, ".toml", new(T), nil)
}

func TestDataDirectoryAndConfigDiscovery(t *testing.T) {
	working := t.TempDir()
	t.Chdir(working)
	t.Setenv("HOME", working)
	for _, key := range []string{"NETWORK", "DATA_DIR", "CONFIG", "RPC_URL", "RPC_URL_FILE"} {
		t.Setenv("LIVEPEER_CHAIN_"+key, "")
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := Root(&out, &out)
		cmd.SetArgs(append(args, "--print-config"))
		err := cmd.Execute()
		return out.String(), err
	}
	_, err := run()
	require.NoError(t, err, "missing default config and empty environment are optional")
	require.NoDirExists(t, filepath.Join(working, ".lpData"))
	_, err = run("--config", "missing.toml")
	require.ErrorContains(t, err, "no such file")
	for i, dir := range []string{filepath.Join(".lpData", "arbitrum-one-mainnet"), "env", "cli"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "chain"), 0700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rpc"), []byte("http://localhost:8545"), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "chain/config.toml"), fmt.Appendf(nil, "Network = 'custom'\nChainID = %d\nRPCURLFile = 'rpc'\n", i+1), 0600))
	}
	for i, args := range [][]string{nil, {"--network", "another"}, {"--data-dir", "cli", "--network", "arbitrum-one-mainnet"}} {
		if i > 0 {
			t.Setenv("LIVEPEER_CHAIN_DATA_DIR", "env")
		}
		out, err := run(args...)
		require.NoError(t, err)
		require.Contains(t, out, fmt.Sprintf("ChainID = %d", i+1))
		if i == 0 {
			require.Contains(t, out, `Network = "custom"`)
		}
	}
	for _, invalid := range []string{"[", "Unknown = true", "DataDir = 'elsewhere'"} {
		require.NoError(t, os.WriteFile("env/chain/config.toml", []byte(invalid), 0600))
		_, err := run()
		require.Error(t, err)
		_, err = run("--config=")
		require.NoError(t, err)
	}
	t.Setenv("HOME", "")
	t.Setenv("LIVEPEER_CHAIN_DATA_DIR", "")
	_, err = run("--data-dir", "cli")
	require.NoError(t, err)
	_, err = run()
	require.ErrorContains(t, err, "data-dir")
}

func TestActionEnvironmentCannotSupplyInputs(t *testing.T) {
	for _, name := range []string{"AMOUNT", "RESERVE", "DELEGATE", "ORCHESTRATOR", "RECIPIENT", "LOCK_ID", "END_ROUND", "REWARD_CUT", "FEE_CUT", "MAX_PRIORITY_FEE_PER_GAS", "TRANSACTION_TIMEOUT", "MAX_TRANSACTION_REPLACEMENTS", "GAS_LIMIT", "SERVICE_URI", "OUTPUT"} {
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
	envAccount := "0x1111111111111111111111111111111111111111"
	cliAccount := "0x2222222222222222222222222222222222222222"
	var wantAccount, chainID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string
			Params []any
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		var result any = "0x0"
		switch request.Method {
		case "eth_chainId":
			result = chainID
		case "eth_getBlockByNumber":
			result = map[string]any{"number": "0x10", "hash": ethcommon.Hash{31: 1}.Hex()}
		case "eth_call":
			result = "0x" + uintWord(0)
			input := request.Params[0].(map[string]any)
			if strings.HasPrefix(input["input"].(string), calldata("getContract(bytes32)")) {
				result = "0x" + addressWord("0x0000000000000000000000000000000000001000")
			}
		case "eth_getBalance":
			require.Equal(t, strings.ToLower(wantAccount), request.Params[0])
		case "eth_getTransactionCount":
			require.Equal(t, []any{strings.ToLower(wantAccount), "pending"}, request.Params)
		default:
			t.Errorf("unexpected RPC method %s", request.Method)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result}))
	}))
	defer server.Close()
	dir := t.TempDir()
	rpcFile := filepath.Join(dir, "rpc")
	require.NoError(t, os.WriteFile(rpcFile, []byte(server.URL), 0600))
	passwordFile := filepath.Join(dir, "password")
	require.NoError(t, os.WriteFile(passwordFile, []byte("unused password"), 0600))
	config := filepath.Join(dir, "chain.toml")
	jsonConfig := filepath.Join(dir, "chain.json")
	for _, path := range []string{config, jsonConfig} {
		test.WriteConfig(t, path, fmt.Sprintf("RPCURLFile = %q\nChainID = 3\nAccount = %q\nMaxFeePerGas = '3'\nKeystoreFile = '/missing/account.json'\nKeystorePasswordFile = %q\n", rpcFile, testAccount, passwordFile))
	}
	partialConfig := filepath.Join(dir, "partial.toml")
	require.NoError(t, os.WriteFile(partialConfig, []byte(fmt.Sprintf("RPCURLFile = %q\n", rpcFile)), 0600))

	for _, tc := range []struct {
		name        string
		args        []string
		env         bool
		account, id string
	}{
		{"config only", []string{"--config", config, "account", "get", "--output", "json"}, false, testAccount, "0x3"},
		{"JSON config", []string{"--config", jsonConfig, "account", "get", "--output", "json"}, false, testAccount, "0x3"},
		{"CLI over JSON", []string{"--config", jsonConfig, "account", "get", "--chain-id", "1", "--account", cliAccount, "--max-fee-per-gas", "1", "--output", "json"}, true, cliAccount, "0x1"},
		{"CLI before leaf", []string{"--config", config, "--chain-id", "1", "--account", cliAccount, "--max-fee-per-gas", "1", "--output", "json", "account", "get"}, true, cliAccount, "0x1"},
		{"CLI after leaf", []string{"account", "get", "--config", config, "--chain-id", "1", "--account", cliAccount, "--max-fee-per-gas", "1", "--output", "json"}, true, cliAccount, "0x1"},
		{"env over config", []string{"--config", config, "account", "get", "--output", "json"}, true, envAccount, "0x2"},
		{"env only", []string{"account", "get", "--output", "json"}, true, envAccount, "0x2"},
		{"CLI over partial config", []string{"account", "get", "--config", partialConfig, "--chain-id", "1", "--account", cliAccount, "--max-fee-per-gas", "1", "--output", "json"}, false, cliAccount, "0x1"},
		{"env over partial config", []string{"account", "get", "--config", partialConfig, "--output", "json"}, true, envAccount, "0x2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range map[string]string{"CHAIN_ID": "2", "ACCOUNT": envAccount, "RPC_URL_FILE": rpcFile, "MAX_FEE_PER_GAS": "2"} {
				if !tc.env {
					value = ""
				}
				t.Setenv("LIVEPEER_CHAIN_"+key, value)
			}
			wantAccount, chainID = ethcommon.HexToAddress(tc.account).Hex(), tc.id
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(tc.args)
			require.NoError(t, root.Execute())
			var account map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &account))
			require.Equal(t, strings.ToLower(wantAccount), account["address"])
			require.Equal(t, "0", account["balance_wei"])
			require.Equal(t, "0", account["lpt_balance_base_units"])
			require.EqualValues(t, 0, account["pending_nonce"])
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

func TestSharedKeystore(t *testing.T) {
	root := t.TempDir()
	key, password := test.WriteKeystore(t, nil)
	data, err := os.ReadFile(key)
	require.NoError(t, err)
	secret, err := os.ReadFile(password)
	require.NoError(t, err)
	dir := filepath.Join(root, "keystore")
	require.NoError(t, os.Mkdir(dir, 0700))
	file := filepath.Join(dir, "account.json")
	require.NoError(t, os.WriteFile(file, data, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "password"), secret, 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "message"), []byte("shared account"), 0600))
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := Root(&out, &out)
		cmd.SetArgs(append([]string{"--data-dir", root, "--config="}, args...))
		err := cmd.Execute()
		return out.String(), err
	}
	_, err = run("sign", "message", "--message-file", "message", "--keystore-password-file", "password")
	require.NoError(t, err)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(data, &metadata))
	account := "0x" + metadata["address"].(string)
	operator := OperatorParams{}
	operator.DataDir = root
	inferred, err := operator.account()
	require.NoError(t, err)
	require.Equal(t, ethcommon.HexToAddress(account), inferred)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "duplicate.json"), data, 0600))
	_, err = run("sign", "message", "--message-file", "message", "--keystore-password-file", "password")
	require.ErrorContains(t, err, "multiple keystore accounts")
	_, err = run("sign", "message", "--keystore-file", "keystore/account.json", "--message-file", "message", "--keystore-password-file", "password")
	require.NoError(t, err)
	_, err = run("--print-config")
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(dir, "duplicate.json")))
	metadata["address"] = "0000000000000000000000000000000000000001"
	data, err = json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, data, 0600))
	_, err = run("sign", "message", "--message-file", "message", "--keystore-password-file", "password")
	require.ErrorContains(t, err, "does not match keystore account")
}

func TestAccountCreateDefaultDirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0755))
	legacy := filepath.Join(root, "lpdb.sqlite3")
	require.NoError(t, os.WriteFile(legacy, []byte("legacy"), 0644))
	password := filepath.Join(root, "password")
	require.NoError(t, os.WriteFile(password, []byte("password"), 0600))
	var out bytes.Buffer
	cmd := Root(&out, &out)
	cmd.SetArgs([]string{"account", "create", "--data-dir", root, "--config=", "--keystore-password-file", "password", "--output", "json"})
	require.NoError(t, cmd.Execute())
	var result map[string]string
	require.NoError(t, json.Unmarshal(out.Bytes(), &result))
	require.NotEmpty(t, result["address"])
	entries, err := os.ReadDir(filepath.Join(root, "keystore"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	info, err := os.Stat(filepath.Join(root, "keystore"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0700), info.Mode().Perm())
	info, err = entries[0].Info()
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	data, err := os.ReadFile(legacy)
	require.NoError(t, err)
	require.Equal(t, "legacy", string(data))
	info, err = os.Stat(root)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0755), info.Mode().Perm())
}
