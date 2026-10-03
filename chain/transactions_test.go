package chain

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

// Quiet execution is covered by the real-process tests in cmd/livepeer.
func TestTransactionCommands(t *testing.T) {
	for _, tc := range []struct {
		name          string
		keySource     int
		flags         []string
		broadcastErr  bool
		receiptErr    bool
		receiptRevert bool
		simulationErr bool
		wantReceipt   bool
		wantErr       string
		wantSent      int
	}{
		{name: "simulate without a key"},
		{name: "simulate with unavailable keystore", flags: []string{"--keystore-file", "/missing/account.json", "--keystore-password-file", "/missing/password"}},
		{name: "explicit submission", flags: []string{"--submit"}, wantSent: 1, wantReceipt: true},
		{name: "keystore environment overrides config", keySource: 1, flags: []string{"--submit"}, wantSent: 1, wantReceipt: true},
		{name: "keystore CLI overrides environment", keySource: 2, flags: []string{"--submit"}, wantSent: 1, wantReceipt: true},
		{name: "receipt failure", flags: []string{"--submit", "--wait"}, receiptErr: true, wantReceipt: true, wantErr: "receipt", wantSent: 1},
		{name: "reverted receipt", flags: []string{"--submit", "--wait"}, receiptRevert: true, wantReceipt: true, wantErr: "reverted", wantSent: 1},
		{name: "failed simulation", flags: []string{"--submit"}, simulationErr: true, wantErr: "simulation failed"},
		{name: "uncertain broadcast", flags: []string{"--submit"}, broadcastErr: true, wantErr: "HTTP 503", wantSent: 1},
		{name: "account differs from keystore", flags: []string{"--submit", "--account", "0x0000000000000000000000000000000000001234"}, wantErr: "configured account address does not match keystore account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			keyFile, passwordPath := test.WriteFixedKeystore(t)
			key, err := eth.OpenKeystoreFile(keyFile, passwordPath)
			require.NoError(t, err)
			f := newManagementFixture(t)
			f.account = key.Address()
			broker := f.addresses["TicketBroker"]
			deposit, ok := new(big.Int).SetString("100000000000000000001", 10)
			require.True(t, ok)
			reserve := big.NewInt(5)
			value := new(big.Int).Add(deposit, reserve)
			data := append(crypto.Keccak256([]byte("fundDepositAndReserve(uint256,uint256)"))[:4], ethcommon.LeftPadBytes(deposit.Bytes(), 32)...)
			data = append(data, ethcommon.LeftPadBytes(reserve.Bytes(), 32)...)
			f.simulationHook = func(from, to ethcommon.Address, input []byte, amount string) {
				require.Equal(t, key.Address(), from)
				require.Equal(t, broker, to)
				require.Equal(t, data, input)
				require.Equal(t, "0x"+value.Text(16), amount)
			}
			if tc.simulationErr {
				f.failSimulation = "fundDepositAndReserve"
			}
			if tc.broadcastErr {
				f.failRPC = "eth_sendRawTransaction"
			}
			if tc.receiptErr {
				f.failRPC = "eth_getTransactionReceipt"
			}
			if tc.receiptRevert {
				f.failReceipt = "fundDepositAndReserve"
			}
			rpcFile := filepath.Join(dir, "rpc")
			require.NoError(t, os.WriteFile(rpcFile, []byte(f.server.URL), 0600))
			configFile := filepath.Join(dir, "chain.toml")
			config := fmt.Sprintf("RPCURLFile = %q\nAccount = %q\n", rpcFile, key.Address().Hex())
			if slices.Contains(tc.flags, "--submit") {
				paths := [3][2]string{{"/missing/key", "/missing/password"}, {"/missing/key", "/missing/password"}, {"/missing/key", "/missing/password"}}
				paths[tc.keySource] = [2]string{keyFile, passwordPath}
				config += fmt.Sprintf("KeystoreFile = %q\nKeystorePasswordFile = %q\n", paths[0][0], paths[0][1])
				if tc.keySource > 0 {
					t.Setenv("LIVEPEER_CHAIN_KEYSTORE_FILE", paths[1][0])
					t.Setenv("LIVEPEER_CHAIN_KEYSTORE_PASSWORD_FILE", paths[1][1])
				}
				if tc.keySource > 1 {
					tc.flags = append(tc.flags, "--keystore-file", paths[2][0], "--keystore-password-file", paths[2][1])
				}
			}
			require.NoError(t, os.WriteFile(configFile, []byte(config), 0600))
			var output bytes.Buffer
			root := Root(&output, &output)
			args := append([]string{"ticketbroker", "fund", "--config", configFile, "--amount", deposit.String(), "--reserve", reserve.String(), "--base-units", "--output", "json"}, tc.flags...)
			root.SetArgs(args)
			err = root.Execute()
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
			sent := len(f.sent)
			var hash ethcommon.Hash
			if sent > 0 {
				tx := f.sent[0]
				hash = tx.Hash()
				require.Equal(t, "42161", tx.ChainId().String())
				require.Equal(t, &broker, tx.To())
				require.Equal(t, value.String(), tx.Value().String())
				require.Equal(t, data, tx.Data())
				require.Zero(t, tx.Nonce())
				recoveredAddress, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
				require.NoError(t, err)
				require.Equal(t, key.Address(), recoveredAddress)
			}
			require.Equal(t, tc.wantSent, sent)
			require.Equal(t, tc.wantReceipt, f.receipts > 0)
			if sent > 0 && tc.wantErr != "" {
				require.ErrorContains(t, err, hash.Hex(), "the operator must be able to reconcile this transaction")
			}
			if tc.wantErr != "" && sent == 0 {
				require.Empty(t, output.String())
				return
			}

			var record struct {
				Submitted       bool   `json:"submitted"`
				TransactionHash string `json:"transaction_hash"`
				Simulation      eth.TransactionPlan
			}
			decoder := json.NewDecoder(&output)
			require.NoError(t, decoder.Decode(&record))
			require.Equal(t, value.String(), record.Simulation.ValueWei)
			require.False(t, record.Submitted)
			require.Empty(t, record.TransactionHash)
			if sent > 0 {
				var ready map[string]any
				require.NoError(t, decoder.Decode(&ready))
				require.Equal(t, hash.Hex(), ready["transaction_hash"])
				require.Equal(t, "ready", ready["broadcast"])
				if !tc.broadcastErr {
					var submitted map[string]any
					require.NoError(t, decoder.Decode(&submitted))
					require.Equal(t, true, submitted["submitted"])
					require.Equal(t, hash.Hex(), submitted["transaction_hash"])
				}
			}

			if tc.wantReceipt && tc.wantErr == "" {
				var receipt map[string]any
				require.NoError(t, decoder.Decode(&receipt))
				require.Equal(t, hash.Hex(), receipt["transaction_hash"])
				require.EqualValues(t, 16, receipt["confirmed_block"])
			}
			require.ErrorIs(t, decoder.Decode(new(any)), io.EOF)
		})
	}
}

func TestBondApprovalSequencing(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		flags                []string
		allowance            int64
		balance              *int64
		failReceipt, wantErr string
		methods              []string
		sent                 int
	}{
		{name: "dry run plans approval", methods: []string{"approve"}},
		{name: "no wait rejects sequence", flags: []string{"--submit", "--no-wait"}, wantErr: "multi-step"},
		{name: "confirms approval before bond", flags: []string{"--submit"}, methods: []string{"approve", "bondWithHint"}, sent: 2},
		{name: "gas and tip overrides on each step", flags: []string{"--submit", "--gas-limit", "50000", "--max-priority-fee-per-gas", "9"}, methods: []string{"approve", "bondWithHint"}, sent: 2},
		{name: "existing allowance", allowance: 5, methods: []string{"bondWithHint"}},
		{name: "insufficient balance", balance: pointer(int64(4)), wantErr: "insufficient Livepeer token balance"},
		{name: "approval reverts", flags: []string{"--submit"}, failReceipt: "approve", methods: []string{"approve"}, sent: 1, wantErr: "reverted"},
		{name: "bond reverts quietly", flags: []string{"--submit", "--quiet"}, failReceipt: "bondWithHint", methods: []string{"approve", "bondWithHint"}, sent: 2, wantErr: "reverted"},
		{name: "quiet approval and bond", flags: []string{"--submit", "--quiet"}, methods: []string{"approve", "bondWithHint"}, sent: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newManagementFixture(t)
			keyFile, passwordPath := test.WriteFixedKeystore(t)
			key, err := eth.OpenKeystoreFile(keyFile, passwordPath)
			require.NoError(t, err)
			f.account = key.Address()
			f.reads["allowance"] = []any{big.NewInt(tc.allowance)}
			if tc.balance != nil {
				f.reads["balanceOf"] = []any{big.NewInt(*tc.balance)}
			}
			f.failReceipt = tc.failReceipt
			bonding, token := f.addresses["BondingManager"], f.addresses["LivepeerToken"]
			approvalData := calldata("approve(address,uint256)", addressWord(bonding.Hex()), uintWord(5))
			bondData := calldata("bondWithHint(uint256,address,address,address,address,address)", uintWord(5), addressWord(testAccount), uintWord(0), uintWord(0), uintWord(0), uintWord(0))
			f.readHook = func(method string, args []any) []any {
				switch method {
				case "balanceOf":
					require.Equal(t, []any{f.account}, args)
				case "allowance":
					require.Equal(t, []any{f.account, bonding}, args)
				}
				return nil
			}
			f.simulationHook = func(from, to ethcommon.Address, data []byte, value string) {
				require.Equal(t, f.account, from)
				expected := bondData
				if to == token {
					expected = approvalData
				} else {
					require.Equal(t, bonding, to)
				}
				require.Equal(t, expected, "0x"+hex.EncodeToString(data))
				require.Equal(t, "0x0", value)
			}
			args := append([]string{"stake", "bond", testAccount, "--amount", "5", "--base-units", "--keystore-file", keyFile, "--keystore-password-file", passwordPath}, tc.flags...)
			out, err := f.run(t, args...)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
				for _, tx := range f.sent {
					require.ErrorContains(t, err, tx.Hash().Hex())
				}
			}
			require.Equal(t, tc.methods, f.simulations)
			require.Len(t, f.sent, tc.sent)
			require.Equal(t, tc.sent, f.receipts)
			for i, tx := range f.sent {
				require.EqualValues(t, i, tx.Nonce())
				if slices.Contains(tc.flags, "--gas-limit") {
					require.Zero(t, f.estimates)
					require.EqualValues(t, 50000, tx.Gas())
					require.Equal(t, big.NewInt(9), tx.GasTipCap())
				} else {
					require.Equal(t, len(f.simulations), f.estimates)
					require.EqualValues(t, 21001+i, tx.Gas())
				}
				expectedTo, expectedData := bonding, bondData
				if i == 0 {
					expectedTo, expectedData = token, approvalData
				}
				require.Equal(t, &expectedTo, tx.To())
				require.Equal(t, expectedData, "0x"+hex.EncodeToString(tx.Data()))
				require.Zero(t, tx.Value().Sign())
			}
			if slices.Contains(tc.flags, "--quiet") || len(tc.methods) == 0 {
				require.Empty(t, out)
			} else {
				require.NotEmpty(t, out)
				if tc.allowance < 5 {
					require.Contains(t, out, "deferred until preceding transactions confirm")
				}
			}
		})
	}
}
