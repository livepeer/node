package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/eth"
)

type action struct {
	Contract string
	Method   string
	Args     []any
	Value    *big.Int
}

func (p OperatorParams) senderAddress() (ethcommon.Address, error) {
	if p.Sender == nil {
		return ethcommon.Address{}, errors.New("sender is required")
	}
	return *p.Sender, nil
}

func formatResult(out io.Writer, mode string, value any) error {
	encoder := json.NewEncoder(out)
	if mode != "json" {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(value)
}

func orchestratorGet(ctx context.Context, p OperatorParams, display DisplayOptions, out io.Writer) error {
	addr, err := p.senderAddress()
	if err != nil {
		return err
	}
	rpc, _, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	defer rpc.Close()
	contracts, err := eth.NewContracts(rpc, p.Controller)
	if err != nil {
		return err
	}
	bonding, err := contracts.Resolve(ctx, "bondingManager")
	if err != nil {
		return err
	}
	registry, err := contracts.Resolve(ctx, "serviceRegistry")
	if err != nil {
		return err
	}
	active, err := contracts.Call(ctx, "bondingManager", bonding, "isActiveTranscoder", addr)
	if err != nil {
		return err
	}
	uri, err := contracts.Call(ctx, "serviceRegistry", registry, "getServiceURI", addr)
	if err != nil {
		return err
	}
	transcoder, err := contracts.Call(ctx, "bondingManager", bonding, "getTranscoder", addr)
	if err != nil {
		return err
	}
	return formatResult(out, display.Output, map[string]any{"address": addr.Hex(), "active": active[0], "service_uri": uri[0], "reward_cut": transcoder[1].(*big.Int).String(), "fee_share": transcoder[2].(*big.Int).String()})
}

// prepareActions produces prerequisite transactions for a particular command.
type prepareActions func(context.Context, *eth.Contracts, ethcommon.Address, []action) ([]action, error)

func prepareBond(ctx context.Context, contracts *eth.Contracts, from ethcommon.Address, actions []action) ([]action, error) {
	bonding, err := contracts.Resolve(ctx, "bondingManager")
	if err != nil {
		return nil, err
	}
	token, err := contracts.Resolve(ctx, "livepeerToken")
	if err != nil {
		return nil, err
	}
	amount := actions[0].Args[0].(*big.Int)
	balances, err := contracts.Call(ctx, "livepeerToken", token, "balanceOf", from)
	if err != nil {
		return nil, err
	}
	if balances[0].(*big.Int).Cmp(amount) < 0 {
		return nil, errors.New("insufficient Livepeer token balance")
	}
	allowances, err := contracts.Call(ctx, "livepeerToken", token, "allowance", from, bonding)
	if err != nil {
		return nil, err
	}
	if allowances[0].(*big.Int).Cmp(amount) < 0 {
		return contractAction("livepeerToken", "approve", bonding, amount), nil
	}
	return nil, nil
}

func executeActions(ctx context.Context, operator OperatorParams, display DisplayOptions, tx TransactionOptions, out io.Writer, command string, actions []action, prepare prepareActions) error {
	if tx.Wait && !tx.Submit {
		return errors.New("wait requires submit")
	}
	if tx.Submit && operator.KeyFile == "" {
		return errors.New("private-key-file is required with submit")
	}
	from, err := operator.senderAddress()
	if err != nil {
		return err
	}
	var key *eth.Key
	if tx.Submit {
		key, err = eth.OpenKeyFile(operator.KeyFile)
		if err != nil {
			return err
		}
		if key.Address() != from {
			return errors.New("sender does not match private key")
		}
	}
	rpc, chainID, err := checkedClient(ctx, operator)
	if err != nil {
		return err
	}
	defer rpc.Close()
	contracts, err := eth.NewContracts(rpc, operator.Controller)
	if err != nil {
		return err
	}
	run := func(a action, nextStep string) error {
		address, err := contracts.Resolve(ctx, a.Contract)
		if err != nil {
			return err
		}
		data, err := contracts.Pack(a.Contract, a.Method, a.Args...)
		if err != nil {
			return err
		}
		plan, err := contracts.PlanTransaction(ctx, from, address, data, a.Value)
		if err != nil {
			return err
		}
		result := map[string]any{"command": command, "contract": a.Contract, "method": a.Method, "simulation": plan, "submitted": false}
		if nextStep != "" {
			result["next_step"] = nextStep
		}
		return submitPlan(ctx, tx, display, out, contracts, key, chainID, plan, result)
	}
	if prepare != nil {
		prerequisites, err := prepare(ctx, contracts, from, actions)
		if err != nil {
			return err
		}
		for _, a := range prerequisites {
			if err := run(a, "bond after approval confirms"); err != nil {
				return err
			}
			if !tx.Submit || !tx.Wait {
				return nil
			}
		}
	}
	for _, a := range actions {
		if err := run(a, ""); err != nil {
			return err
		}
	}
	return nil
}

// Unless quiet, emit the submission record before waiting. Errors retain the
// locally computed hash so even uncertain sends can be reconciled.
func submitPlan(ctx context.Context, tx TransactionOptions, display DisplayOptions, out io.Writer, contracts *eth.Contracts, key *eth.Key, chainID *big.Int, plan eth.TransactionPlan, result map[string]any) error {
	if !tx.Submit {
		return formatResult(out, display.Output, result)
	}
	hash, sendErr := contracts.Submit(ctx, plan, key, chainID)
	if hash != (ethcommon.Hash{}) {
		result["transaction_hash"] = hash.Hex()
	}
	result["submitted"] = sendErr == nil
	if sendErr != nil {
		result["submission_error"] = sendErr.Error()
	}
	if err := errors.Join(formatResult(out, display.Output, result), sendErr); err != nil {
		if hash == (ethcommon.Hash{}) {
			return err
		}
		return fmt.Errorf("transaction %s: %w", hash.Hex(), err)
	}
	if !tx.Wait {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	block, err := contracts.WaitReceipt(waitCtx, hash)
	if err != nil {
		return fmt.Errorf("transaction %s receipt: %w", hash.Hex(), err)
	}
	return formatResult(out, display.Output, map[string]any{"transaction_hash": hash.Hex(), "confirmed_block": block})
}
