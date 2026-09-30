package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
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

func parseUint(value, name string, required bool) (*big.Int, error) {
	if value == "" && !required {
		return nil, nil
	}
	v, ok := new(big.Int).SetString(value, 10)
	if !ok || v.Sign() < 0 || v.BitLen() > 256 {
		return nil, fmt.Errorf("%s must be an unsigned decimal integer", name)
	}
	return v, nil
}

func parseAddress(value, name string) (ethcommon.Address, error) {
	if !eth.ValidAddress(value) {
		return ethcommon.Address{}, fmt.Errorf("%s must be a 20-byte Ethereum address", name)
	}
	return ethcommon.HexToAddress(value), nil
}

func (p Params) senderAddress() (ethcommon.Address, error) { return parseAddress(p.Sender, "sender") }

func buildActions(command string, p Params) ([]action, error) {
	zero := big.NewInt(0)
	amount := func() (*big.Int, error) {
		v, err := parseUint(p.Amount, "amount", true)
		if err != nil {
			return nil, err
		}
		if v.Sign() == 0 {
			return nil, errors.New("amount must be positive")
		}
		return v, nil
	}
	lock := func() (*big.Int, error) { return parseUint(p.LockID, "lock-id", true) }
	switch command {
	case "orchestrator activate":
		cut, err := parseUint(p.RewardCut, "reward-cut", true)
		if err != nil {
			return nil, err
		}
		share, err := parseUint(p.FeeShare, "fee-share", true)
		if err != nil {
			return nil, err
		}
		if cut.Cmp(big.NewInt(1_000_000)) > 0 || share.Cmp(big.NewInt(1_000_000)) > 0 {
			return nil, errors.New("reward-cut and fee-share must be at most 1000000")
		}
		return []action{{"bondingManager", "transcoder", []any{cut, share}, zero}}, nil
	case "orchestrator set-config":
		var result []action
		if p.RewardCut != "" || p.FeeShare != "" {
			cut, err := parseUint(p.RewardCut, "reward-cut", true)
			if err != nil {
				return nil, err
			}
			share, err := parseUint(p.FeeShare, "fee-share", true)
			if err != nil {
				return nil, err
			}
			if cut.Cmp(big.NewInt(1_000_000)) > 0 || share.Cmp(big.NewInt(1_000_000)) > 0 {
				return nil, errors.New("reward-cut and fee-share must be at most 1000000")
			}
			result = append(result, action{"bondingManager", "transcoder", []any{cut, share}, zero})
		}
		if p.ServiceURI != "" {
			if !(strings.HasPrefix(p.ServiceURI, "https://") || strings.HasPrefix(p.ServiceURI, "http://")) {
				return nil, errors.New("service-uri must be an absolute HTTP or HTTPS URL")
			}
			result = append(result, action{"serviceRegistry", "setServiceURI", []any{p.ServiceURI}, zero})
		}
		if len(result) == 0 {
			return nil, errors.New("reward-cut and fee-share or service-uri is required")
		}
		return result, nil
	case "orchestrator reward":
		return []action{{"bondingManager", "reward", nil, zero}}, nil
	case "stake bond":
		v, err := amount()
		if err != nil {
			return nil, err
		}
		delegate, err := parseAddress(p.Delegate, "delegate")
		if err != nil {
			return nil, err
		}
		return []action{{"bondingManager", "bond", []any{v, delegate}, zero}}, nil
	case "stake unbond":
		v, err := amount()
		if err != nil {
			return nil, err
		}
		return []action{{"bondingManager", "unbond", []any{v}, zero}}, nil
	case "stake rebond":
		v, err := lock()
		if err != nil {
			return nil, err
		}
		return []action{{"bondingManager", "rebond", []any{v}, zero}}, nil
	case "stake withdraw":
		v, err := lock()
		if err != nil {
			return nil, err
		}
		return []action{{"bondingManager", "withdrawStake", []any{v}, zero}}, nil
	case "earnings claim":
		v, err := parseUint(p.EndRound, "end-round", true)
		if err != nil {
			return nil, err
		}
		return []action{{"bondingManager", "claimEarnings", []any{v}, zero}}, nil
	case "earnings withdraw-fees":
		v, err := amount()
		if err != nil {
			return nil, err
		}
		recipient, err := parseAddress(p.Recipient, "recipient")
		if err != nil {
			return nil, err
		}
		return []action{{"bondingManager", "withdrawFees", []any{recipient, v}, zero}}, nil
	case "ticketbroker fund":
		deposit, err := parseUint(p.Amount, "amount", true)
		if err != nil {
			return nil, err
		}
		reserve, err := parseUint(p.Reserve, "reserve", true)
		if err != nil {
			return nil, err
		}
		value := new(big.Int).Add(deposit, reserve)
		if value.Sign() == 0 {
			return nil, errors.New("deposit and reserve cannot both be zero")
		}
		return []action{{"ticketBroker", "fundDepositAndReserve", []any{deposit, reserve}, value}}, nil
	case "ticketbroker unlock":
		return []action{{"ticketBroker", "unlock", nil, zero}}, nil
	case "ticketbroker cancel-unlock":
		return []action{{"ticketBroker", "cancelUnlock", nil, zero}}, nil
	case "ticketbroker withdraw":
		return []action{{"ticketBroker", "withdraw", nil, zero}}, nil
	case "round initialize":
		return []action{{"roundsManager", "initializeRound", nil, zero}}, nil
	default:
		return nil, errors.New("unsupported chain command")
	}
}

func formatResult(out io.Writer, mode string, value any) error {
	if mode == "json" {
		return json.NewEncoder(out).Encode(value)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}

func orchestratorGet(ctx context.Context, p Params, out io.Writer) error {
	addr, err := p.senderAddress()
	if err != nil {
		return err
	}
	rpc, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	contracts, err := eth.OpenContracts(rpc, p.Controller)
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
	return formatResult(out, p.Output, map[string]any{"address": addr.Hex(), "active": active[0], "service_uri": uri[0], "reward_cut": transcoder[1].(*big.Int).String(), "fee_share": transcoder[2].(*big.Int).String()})
}

func executeAction(ctx context.Context, p Params, out io.Writer, command string) error {
	from, err := p.senderAddress()
	if err != nil {
		return err
	}
	rpc, err := checkedClient(ctx, p)
	if err != nil {
		return err
	}
	contracts, err := eth.OpenContracts(rpc, p.Controller)
	if err != nil {
		return err
	}
	actions, err := buildActions(command, p)
	if err != nil {
		return err
	}
	var key *eth.Key
	if p.Submit {
		key, err = eth.OpenKeyFile(p.KeyFile)
		if err != nil {
			return err
		}
		if key.Address() != from {
			return errors.New("sender does not match private key")
		}
	}
	if p.Wait && !p.Submit {
		return errors.New("wait requires submit")
	}
	chainID, _ := new(big.Int).SetString(p.ChainID, 10)
	if command == "stake bond" {
		bonding, err := contracts.Resolve(ctx, "bondingManager")
		if err != nil {
			return err
		}
		token, err := contracts.Resolve(ctx, "livepeerToken")
		if err != nil {
			return err
		}
		balanceValues, err := contracts.Call(ctx, "livepeerToken", token, "balanceOf", from)
		if err != nil {
			return err
		}
		balance, ok := balanceValues[0].(*big.Int)
		if !ok || balance.Cmp(actions[0].Args[0].(*big.Int)) < 0 {
			return errors.New("insufficient Livepeer token balance")
		}
		allowanceValues, err := contracts.Call(ctx, "livepeerToken", token, "allowance", from, bonding)
		if err != nil {
			return err
		}
		allowance, ok := allowanceValues[0].(*big.Int)
		if !ok {
			return errors.New("invalid Livepeer token allowance")
		}
		if allowance.Cmp(actions[0].Args[0].(*big.Int)) < 0 {
			data, err := contracts.Pack("livepeerToken", "approve", bonding, actions[0].Args[0].(*big.Int))
			if err != nil {
				return err
			}
			plan, err := contracts.PlanTransaction(ctx, from, token, data, big.NewInt(0))
			if err != nil {
				return err
			}
			result := map[string]any{"command": "stake bond", "contract": "livepeerToken", "method": "approve", "simulation": plan, "submitted": false, "next_step": "bond after approval confirms"}
			if err := submitPlan(ctx, p, out, contracts, key, chainID, plan, result); err != nil {
				return err
			}
			if !p.Submit || !p.Wait {
				return nil
			}

		}
	}
	for _, a := range actions {
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
		if err := submitPlan(ctx, p, out, contracts, key, chainID, plan, result); err != nil {
			return err
		}
	}
	return nil
}

func addContractCommands(_ any, add func(string, string, func(context.Context, Params, io.Writer) error)) {
	add("orchestrator get", "Read orchestrator status and configuration", orchestratorGet)
	for _, cmd := range []string{
		"orchestrator activate", "orchestrator set-config", "orchestrator reward",
		"stake bond", "stake unbond", "stake rebond", "stake withdraw",
		"earnings claim", "earnings withdraw-fees",
		"ticketbroker fund", "ticketbroker unlock", "ticketbroker cancel-unlock", "ticketbroker withdraw",
		"round initialize",
	} {
		name := cmd
		add(name, "Simulate or explicitly submit "+name, func(ctx context.Context, p Params, out io.Writer) error { return executeAction(ctx, p, out, name) })
	}
}

// Emit a submission record before any wait. Even an uncertain send carries the
// locally computed hash, so operators can reconcile it without signing again.
func submitPlan(ctx context.Context, p Params, out io.Writer, contracts *eth.Contracts, key *eth.Key, chainID *big.Int, plan eth.TransactionPlan, result map[string]any) error {
	if !p.Submit {
		return formatResult(out, p.Output, result)
	}
	hash, sendErr := contracts.Submit(ctx, plan, key, chainID)
	if hash != (ethcommon.Hash{}) {
		result["transaction_hash"] = hash.Hex()
	}
	result["submitted"] = sendErr == nil
	if sendErr != nil {
		result["submission_error"] = sendErr.Error()
	}
	if err := formatResult(out, p.Output, result); err != nil {
		return errors.Join(err, sendErr)
	}
	if sendErr != nil {
		return fmt.Errorf("transaction %s: %w", hash.Hex(), sendErr)
	}
	if !p.Wait {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	block, err := contracts.WaitReceipt(waitCtx, hash)
	if err != nil {
		return fmt.Errorf("transaction %s receipt: %w", hash.Hex(), err)
	}
	return formatResult(out, p.Output, map[string]any{"transaction_hash": hash.Hex(), "confirmed_block": block})
}
