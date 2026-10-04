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
		{[]string{"ticketbroker", "unlock", "--submit"}, "keystore-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-file", path}, "keystore-password-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-password-file", passwordPath}, "keystore-file"},
		{[]string{"ticketbroker", "unlock", "--submit", "--keystore-file", path, "--keystore-password-file", passwordPath}, "cannot decrypt keystore"},
		{[]string{"status", "--submit"}, "unknown flag"},
		{[]string{"account", "--quiet"}, "unknown flag"},
		{[]string{"account", "create"}, "are required"},
		{[]string{"account", "create", "extra"}, "unknown command"},
		{[]string{"status", "--chain-id", "0"}, "below min"},
		{[]string{"status", "--controller-address", "0x0000000000000000000000000000000000000000"}, "must be nonzero"},
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
	t.Setenv("LIVEPEER_CHAIN_ACCOUNT", "")
	t.Setenv("LIVEPEER_CHAIN_RPC_URL", "http://localhost:1")
	for _, command := range []string{"account get", "orchestrator get", "ticketbroker unlock"} {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			root := Root(&output, &output)
			root.SetArgs(strings.Fields(command))
			require.ErrorContains(t, root.Execute(), "account address is required")
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
	require.NoError(t, os.WriteFile(config, []byte(fmt.Sprintf("RPCURLFile = %q\nChainID = 3\nAccount = %q\nMaxFeePerGas = '3'\nKeystoreFile = '/missing/account.json'\nKeystorePasswordFile = %q\n", rpcFile, testAccount, passwordFile)), 0600))
	partialConfig := filepath.Join(dir, "partial.toml")
	require.NoError(t, os.WriteFile(partialConfig, []byte(fmt.Sprintf("RPCURLFile = %q\n", rpcFile)), 0600))

	for _, tc := range []struct {
		name        string
		args        []string
		env         bool
		account, id string
	}{
		{"config only", []string{"--config", config, "account", "get", "--output", "json"}, false, testAccount, "0x3"},
		{"CLI before leaf", []string{"--config", config, "--chain-id", "1", "--account", cliAccount, "--max-fee-per-gas", "1", "--output", "json", "account", "get"}, true, cliAccount, "0x1"},
		{"CLI after leaf", []string{"account", "get", "--config", config, "--chain-id", "1", "--account", cliAccount, "--max-fee-per-gas", "1", "--output", "json"}, true, cliAccount, "0x1"},
		{"env over config", []string{"--config", config, "account", "get", "--output", "json"}, true, envAccount, "0x2"},
		{"env only", []string{"account", "get", "--output", "json"}, true, envAccount, "0x2"},
		{"CLI over partial config", []string{"account", "get", "--config", partialConfig, "--chain-id", "1", "--account", cliAccount, "--max-fee-per-gas", "1", "--output", "json"}, false, cliAccount, "0x1"},
		{"env over partial config", []string{"account", "get", "--config", partialConfig, "--output", "json"}, true, envAccount, "0x2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LIVEPEER_CHAIN_CHAIN_ID", "")
			t.Setenv("LIVEPEER_CHAIN_ACCOUNT", "")
			t.Setenv("LIVEPEER_CHAIN_RPC_URL_FILE", "")
			t.Setenv("LIVEPEER_CHAIN_MAX_FEE_PER_GAS", "")
			if tc.env {
				t.Setenv("LIVEPEER_CHAIN_CHAIN_ID", "2")
				t.Setenv("LIVEPEER_CHAIN_ACCOUNT", envAccount)
				t.Setenv("LIVEPEER_CHAIN_RPC_URL_FILE", rpcFile)
				t.Setenv("LIVEPEER_CHAIN_MAX_FEE_PER_GAS", "2")
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
