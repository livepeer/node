package eth

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
)

// FeeOptions applies to one transaction. A zero gas limit requests estimation.
type FeeOptions struct {
	GasLimit    uint64
	PriorityFee *big.Int
}

type FeeInspection struct {
	BlockSnapshot
	BaseFeeWei      string  `json:"base_fee_per_gas_wei"`
	SuggestedTipWei *string `json:"suggested_priority_fee_per_gas_wei"`
	TipCapWei       string  `json:"max_priority_fee_per_gas_wei"`
	FeeCapWei       string  `json:"calculated_fee_cap_wei"`
	CeilingWei      *string `json:"fee_ceiling_wei"`
}

func (c *Contracts) Fees(ctx context.Context, initialTip *big.Int) (FeeInspection, error) {
	return c.fees(ctx, initialTip, false)
}

// InspectFees also reports the RPC suggestion when an explicit tip is selected.
func (c *Contracts) InspectFees(ctx context.Context, initialTip *big.Int) (FeeInspection, error) {
	return c.fees(ctx, initialTip, true)
}
func (c *Contracts) fees(ctx context.Context, initialTip *big.Int, inspect bool) (FeeInspection, error) {
	header, err := c.RPC.header(ctx, "latest")
	if err != nil {
		return FeeInspection{}, err
	}
	if header.BaseFee == nil {
		return FeeInspection{}, errMissingBaseFee
	}
	if (*big.Int)(header.BaseFee).Sign() < 0 {
		return FeeInspection{}, errors.New("invalid base fee")
	}
	if header.Number == nil || (*big.Int)(header.Number).Sign() < 0 || header.Hash == (common.Hash{}) {
		return FeeInspection{}, errors.New("invalid fee block")
	}
	result := FeeInspection{BlockSnapshot: BlockSnapshot{BlockNumber: (*big.Int)(header.Number).String(), BlockHash: header.Hash}, BaseFeeWei: (*big.Int)(header.BaseFee).String()}
	var tip *big.Int
	if initialTip == nil || inspect {
		tip, err = c.RPC.ethereum.SuggestGasTipCap(ctx)
		if err != nil {
			var rpcErr gethrpc.Error
			if !errors.As(err, &rpcErr) || (rpcErr.ErrorCode() != -32601 && rpcErr.ErrorCode() != -32004) {
				return FeeInspection{}, safeRPCError(err)
			}
			price, priceErr := c.RPC.ethereum.SuggestGasPrice(ctx)
			if priceErr != nil {
				return FeeInspection{}, safeRPCError(priceErr)
			}
			tip = new(big.Int).Sub(price, (*big.Int)(header.BaseFee))
		}
		if tip == nil || tip.Sign() < 0 || tip.BitLen() > 256 {
			return FeeInspection{}, errors.New("invalid suggested priority fee")
		}
		suggestion := tip.String()
		result.SuggestedTipWei = &suggestion
	}
	if initialTip != nil {
		tip = new(big.Int).Set(initialTip)
	}
	if tip.Sign() < 0 || tip.BitLen() > 256 {
		return FeeInspection{}, errors.New("invalid priority fee")
	}
	fee := new(big.Int).Add(new(big.Int).Mul((*big.Int)(header.BaseFee), big.NewInt(2)), tip)
	if fee.BitLen() > 256 {
		return FeeInspection{}, errors.New("transaction fee overflows uint256")
	}
	result.TipCapWei, result.FeeCapWei = tip.String(), fee.String()
	if c.MaxFeePerGas != nil {
		ceiling := c.MaxFeePerGas.String()
		result.CeilingWei = &ceiling
	}
	return result, c.checkFee(fee)
}

// WaitOptions controls CLI inclusion waiting. Payment finality and durable
// recovery do not use this API. Timeout applies to every attempt, including last.
type WaitOptions struct {
	Timeout         time.Duration
	MaxReplacements uint64
}

type Confirmation struct {
	Hash     common.Hash   `json:"transaction_hash"`
	Block    uint64        `json:"confirmed_block"`
	Attempts []common.Hash `json:"attempt_hashes"`
}

func bumpFee(value *big.Int) *big.Int {
	// ceil(value * 1.11), with a strict increase even from zero.
	bumped := new(big.Int).Div(new(big.Int).Add(new(big.Int).Mul(value, big.NewInt(111)), big.NewInt(99)), big.NewInt(100))
	if bumped.Cmp(value) <= 0 {
		bumped.Add(value, big.NewInt(1))
	}
	return bumped
}

func (c *Contracts) replacement(ctx context.Context, previous SignedTransaction, key *Key) (SignedTransaction, TransactionPlan, error) {
	var old types.Transaction
	if err := old.UnmarshalBinary(previous.Raw); err != nil || old.Hash() != previous.Hash || old.Nonce() != previous.Nonce || old.To() == nil || old.Type() != types.DynamicFeeTxType {
		return SignedTransaction{}, TransactionPlan{}, errors.New("invalid replacement identity")
	}
	if key == nil || key.Address() != previous.From {
		return SignedTransaction{}, TransactionPlan{}, errors.New("transaction account does not match signing key")
	}
	recoveredAddress, err := types.Sender(types.LatestSignerForChainID(old.ChainId()), &old)
	if err != nil || recoveredAddress != previous.From {
		return SignedTransaction{}, TransactionPlan{}, errors.New("replacement transaction account mismatch")
	}
	fees, err := c.Fees(ctx, nil)
	if err != nil {
		return SignedTransaction{}, TransactionPlan{}, err
	}
	tip := bumpFee(old.GasTipCap())
	freshTip, _ := new(big.Int).SetString(fees.TipCapWei, 10)
	if tip.Cmp(freshTip) < 0 {
		tip = freshTip
	}
	base, _ := new(big.Int).SetString(fees.BaseFeeWei, 10)
	fee := bumpFee(old.GasFeeCap())
	freshFee := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tip)
	if fee.Cmp(freshFee) < 0 {
		fee = freshFee
	}
	if tip.BitLen() > 256 || fee.BitLen() > 256 {
		return SignedTransaction{}, TransactionPlan{}, errors.New("replacement fee overflows uint256")
	}
	if err := c.checkFee(fee); err != nil {
		return SignedTransaction{}, TransactionPlan{}, err
	}
	tx := types.NewTx(&types.DynamicFeeTx{ChainID: old.ChainId(), Nonce: old.Nonce(), To: old.To(), Value: old.Value(), Data: old.Data(), Gas: old.Gas(), GasFeeCap: fee, GasTipCap: tip, AccessList: old.AccessList()})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(old.ChainId()), key.private)
	if err != nil {
		return SignedTransaction{}, TransactionPlan{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return SignedTransaction{}, TransactionPlan{}, err
	}
	plan := TransactionPlan{From: previous.From, To: *old.To(), ValueWei: old.Value().String(), Data: "0x" + fmt.Sprintf("%x", old.Data()), GasLimit: old.Gas(), FeeCapWei: fee.String(), TipCapWei: tip.String()}
	return SignedTransaction{Hash: signed.Hash(), Raw: raw, Nonce: old.Nonce(), From: previous.From}, plan, nil
}

// WaitConfirmation retains the signed identity and watches every attempt. The
// callback runs before each replacement broadcast, allowing callers to report
// its hash and resolved fees even if a send has an uncertain outcome.
func (c *Contracts) WaitConfirmation(ctx context.Context, first SignedTransaction, key *Key, options WaitOptions, beforeBroadcast func(SignedTransaction, TransactionPlan) error) (Confirmation, error) {
	result := Confirmation{Attempts: []common.Hash{first.Hash}}
	if options.Timeout <= 0 {
		return result, errors.New("transaction timeout must be positive")
	}
	previous := first
	for attempt := uint64(0); ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, options.Timeout)
		err := c.waitAttempts(attemptCtx, &result)
		timedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if !timedOut || !errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		if attempt == options.MaxReplacements {
			return result, fmt.Errorf("transaction confirmation timed out after %d attempt(s): %w", len(result.Attempts), err)
		}
		next, plan, err := c.replacement(ctx, previous, key)
		if err != nil {
			return result, err
		}
		result.Attempts = append(result.Attempts, next.Hash)
		if beforeBroadcast != nil {
			if err := beforeBroadcast(next, plan); err != nil {
				return result, err
			}
		}
		if err := c.Broadcast(ctx, next); err != nil {
			return result, err
		}
		previous = next
	}
}

func (c *Contracts) waitAttempts(ctx context.Context, result *Confirmation) error {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		for _, hash := range result.Attempts {
			receipt, err := c.RPC.TransactionReceipt(ctx, hash)
			if err == nil {
				result.Hash, result.Block = hash, receipt.BlockNumber.Uint64()
				if receipt.Status != types.ReceiptStatusSuccessful {
					return errors.New("transaction reverted")
				}
				return nil
			}
			if !errors.Is(err, ethereum.NotFound) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
