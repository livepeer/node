package chain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"path/filepath"
	"strings"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/nodeconfig"
)

type action struct {
	Contract string
	Method   string
	Args     []any
	Value    *big.Int
	Address  *ethcommon.Address
	resolve  func(context.Context, *eth.Contracts, ethcommon.Address) ([]action, error)
}

func (p OperatorParams) account() (ethcommon.Address, error) {
	if p.Account == nil {
		_, address, err := eth.SelectKeystore(filepath.Join(p.DataDir, "keystore"), nodeconfig.Path(p.DataDir, p.KeystoreFile), nil)
		return address, err
	}
	return *p.Account, nil
}

func executeActions(ctx context.Context, operator OperatorParams, display DisplayOptions, tx TransactionOptions, out io.Writer, command string, actions []action) error {
	if tx.TransactionTimeout <= 0 {
		return errors.New("--transaction-timeout must be positive")
	}

	wait := tx.Wait && !tx.NoWait
	if !wait && tx.MaxTransactionReplacements > 0 {
		return errors.New("--max-transaction-replacements requires waiting; omit --no-wait and use --wait=true")
	}
	if tx.GasLimit != nil && *tx.GasLimit == 0 {
		return errors.New("--gas-limit must be positive")
	}
	if tx.Submit && operator.KeystorePassword == nil && operator.KeystorePasswordFile == "" {
		return errors.New("--submit requires LIVEPEER_CHAIN_KEYSTORE_PASSWORD or --keystore-password-file (or LIVEPEER_CHAIN_KEYSTORE_PASSWORD_FILE)")
	}
	from, err := operator.account()
	if err != nil {
		return fmt.Errorf("select chain account (set --account or check --keystore-file and --data-dir): %w", err)
	}
	var key *eth.Key
	if tx.Submit {
		key, err = eth.OpenAccount(filepath.Join(operator.DataDir, "keystore"), nodeconfig.Path(operator.DataDir, operator.KeystoreFile), operator.KeystorePassword, operator.KeystorePasswordFile, &from)
		if err != nil {
			return fmt.Errorf("open chain keystore (check --keystore-file or --data-dir and --account, and LIVEPEER_CHAIN_KEYSTORE_PASSWORD or --keystore-password-file): %w", err)
		}
	}
	rpc, chainID, err := checkedClient(ctx, operator)
	if err != nil {
		return err
	}
	defer rpc.Close()
	contracts, err := eth.NewContracts(rpc, operator.Controller)
	if err != nil {
		return fmt.Errorf("initialize chain contract bindings: %w", err)
	}
	if operator.MaxFeePerGas != nil {
		contracts.MaxFeePerGas = operator.MaxFeePerGas.ToBig()
	}
	var resolved []action
	for _, a := range actions {
		if a.resolve != nil {
			expanded, err := a.resolve(ctx, contracts, from)
			if err != nil {
				return err
			}
			resolved = append(resolved, expanded...)
		} else {
			resolved = append(resolved, a)
		}
	}
	if tx.Submit && !wait && len(resolved) > 1 {
		return errors.New("--no-wait and --wait=false are not supported for multi-step submissions; use --wait=true")
	}
	if len(resolved) == 0 {
		return formatResult(out, display.Output, map[string]any{"command": command, "no_op": true, "success": true, "message": "configuration is already set"})
	}
	options := eth.FeeOptions{}
	if tx.GasLimit != nil {
		options.GasLimit = *tx.GasLimit
	}
	if tx.MaxPriorityFeePerGas != nil {
		options.PriorityFee = tx.MaxPriorityFeePerGas.ToBig()
	}
	planAction := func(a action) (eth.TransactionPlan, error) {
		if a.Method == "changeDelegate" {
			return contracts.ChangeDelegateWithOptions(ctx, from, a.Args[0].(ethcommon.Address), options)
		}
		if a.Address != nil {
			data, err := contracts.Pack(a.Contract, a.Method, a.Args...)
			if err != nil {
				return eth.TransactionPlan{}, err
			}
			return contracts.PlanTransactionWithOptions(ctx, from, *a.Address, data, a.Value, options)
		}
		return contracts.PlanContractWithOptions(ctx, from, a.Contract, a.Method, a.Value, options, a.Args...)
	}
	// Show the complete sequence before the first broadcast. Later steps must
	// be simulated against the confirmed state established by their predecessors.
	var initial eth.TransactionPlan
	for i, a := range resolved {
		result := map[string]any{"command": command, "step": i + 1, "steps": len(resolved), "contract": a.Contract, "method": a.Method, "submitted": false}
		if i == 0 {
			initial, err = planAction(a)
			if err != nil {
				return err
			}
			result["simulation"] = initial
		} else {
			result["simulation"] = "deferred until preceding transactions confirm"
			result["gas_estimate"] = "deferred"
			if tx.GasLimit != nil {
				result["gas_limit"] = *tx.GasLimit
			}
			result["arguments"] = a.Args
		}
		if err := formatResult(out, display.Output, result); err != nil {
			return err
		}
	}
	if !tx.Submit {
		return nil
	}
	var hashes []string
	for i, a := range resolved {
		plan := initial
		if i > 0 {
			plan, err = planAction(a)
		}
		if err == nil {
			var attempts []ethcommon.Hash
			attempts, err = submitPlan(ctx, tx, display, out, contracts, key, chainID, plan, map[string]any{"command": command, "step": i + 1, "contract": a.Contract, "method": a.Method})
			for _, hash := range attempts {
				hashes = append(hashes, hash.Hex())
			}
		}
		if err != nil {
			if len(hashes) > 0 {
				err = fmt.Errorf("transactions %s: %w", strings.Join(hashes, ", "), err)
			}
			return fmt.Errorf("step %d of %d (%s.%s): %w", i+1, len(resolved), a.Contract, a.Method, err)
		}
	}
	return nil
}

func submitPlan(ctx context.Context, tx TransactionOptions, display DisplayOptions, out io.Writer, contracts *eth.Contracts, key *eth.Key, chainID *big.Int, plan eth.TransactionPlan, result map[string]any) ([]ethcommon.Hash, error) {
	signed, err := contracts.Prepare(ctx, plan, key, chainID)
	if err != nil {
		return nil, err
	}
	attempts := []ethcommon.Hash{signed.Hash}
	report := func(s eth.SignedTransaction, p eth.TransactionPlan) error {
		return formatResult(out, display.Output, map[string]any{"transaction_hash": s.Hash.Hex(), "gas_limit": p.GasLimit, "fee_cap_wei": p.FeeCapWei, "tip_cap_wei": p.TipCapWei, "broadcast": "ready"})
	}
	if err := report(signed, plan); err != nil {
		return attempts, err
	}
	if err := contracts.Broadcast(ctx, signed); err != nil {
		return attempts, err
	}
	result["transaction_hash"], result["submitted"] = signed.Hash.Hex(), true
	if err := formatResult(out, display.Output, result); err != nil {
		return attempts, err
	}
	if tx.NoWait || !tx.Wait {
		return attempts, nil
	}
	confirmation, err := contracts.WaitConfirmation(ctx, signed, key, eth.WaitOptions{Timeout: tx.TransactionTimeout, MaxReplacements: tx.MaxTransactionReplacements}, report)
	if err != nil {
		return confirmation.Attempts, fmt.Errorf("receipt: %w", err)
	}
	return confirmation.Attempts, formatResult(out, display.Output, confirmation)
}
