package eth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// Contracts is the narrow ABI and RPC surface used by the standalone chain
// command and payment redemption. Contract addresses are resolved through the
// deployed Livepeer Controller, as in go-livepeer/eth/client.go.
type Contracts struct {
	RPC        *RPC
	Controller ethcommon.Address
	abis       map[string]abi.ABI
}

func OpenContracts(rpc *RPC, controller string) (*Contracts, error) {
	if !ValidAddress(controller) {
		return nil, errors.New("controller-address must be a 20-byte Ethereum address")
	}
	c := &Contracts{RPC: rpc, Controller: ethcommon.HexToAddress(controller), abis: map[string]abi.ABI{}}
	for name, source := range contractABIs {
		parsed, err := abi.JSON(strings.NewReader(source))
		if err != nil {
			return nil, fmt.Errorf("invalid %s ABI: %w", name, err)
		}
		c.abis[name] = parsed
	}
	return c, nil
}

func (c *Contracts) Resolve(ctx context.Context, name string) (ethcommon.Address, error) {
	if _, ok := c.abis[name]; !ok {
		return ethcommon.Address{}, errors.New("unsupported contract")
	}
	if name == "controller" {
		return c.Controller, nil
	}
	contractName := map[string]string{"bondingManager": "BondingManager", "ticketBroker": "TicketBroker", "roundsManager": "RoundsManager", "serviceRegistry": "ServiceRegistry", "livepeerToken": "LivepeerToken"}[name]
	id := crypto.Keccak256Hash([]byte(contractName))
	values, err := c.Call(ctx, "controller", c.Controller, "getContract", id)
	if err != nil {
		return ethcommon.Address{}, err
	}
	address, ok := values[0].(ethcommon.Address)
	if !ok || address == (ethcommon.Address{}) {
		return ethcommon.Address{}, fmt.Errorf("%s is not registered in Controller", contractName)
	}
	return address, nil
}

func (c *Contracts) Pack(name, method string, args ...any) ([]byte, error) {
	contract, ok := c.abis[name]
	if !ok {
		return nil, errors.New("unsupported contract")
	}
	if _, ok := contract.Methods[method]; !ok {
		return nil, errors.New("unsupported contract method")
	}
	return contract.Pack(method, args...)
}

func (c *Contracts) Call(ctx context.Context, name string, address ethcommon.Address, method string, args ...any) ([]any, error) {
	data, err := c.Pack(name, method, args...)
	if err != nil {
		return nil, err
	}
	raw, err := c.RPC.CallString(ctx, "eth_call", map[string]string{"to": address.Hex(), "data": "0x" + hex.EncodeToString(data)}, "latest")
	if err != nil {
		return nil, err
	}
	result, err := hex.DecodeString(strings.TrimPrefix(raw, "0x"))
	if err != nil {
		return nil, errors.New("invalid contract response")
	}
	values, err := c.abis[name].Unpack(method, result)
	if err != nil {
		return nil, fmt.Errorf("invalid %s.%s result: %w", name, method, err)
	}
	return values, nil
}

type TransactionPlan struct {
	To          ethcommon.Address `json:"to"`
	From        ethcommon.Address `json:"from"`
	Data        string            `json:"data"`
	ValueWei    string            `json:"value_wei"`
	GasLimit    uint64            `json:"gas_limit"`
	GasPriceWei string            `json:"gas_price_wei"`
}

func hexQuantity(value *big.Int) string { return "0x" + value.Text(16) }

// PlanTransaction simulates a state change and estimates gas before any signing.
func (c *Contracts) PlanTransaction(ctx context.Context, from, to ethcommon.Address, data []byte, value *big.Int) (TransactionPlan, error) {
	if value == nil || value.Sign() < 0 {
		return TransactionPlan{}, errors.New("invalid transaction value")
	}
	call := map[string]string{"from": from.Hex(), "to": to.Hex(), "data": "0x" + hex.EncodeToString(data), "value": hexQuantity(value)}
	if _, err := c.RPC.CallString(ctx, "eth_call", call, "pending"); err != nil {
		return TransactionPlan{}, fmt.Errorf("transaction simulation failed: %w", err)
	}
	gasRaw, err := c.RPC.CallString(ctx, "eth_estimateGas", call)
	if err != nil {
		return TransactionPlan{}, err
	}
	gas, err := ParseHexQuantity(gasRaw)
	if err != nil || !gas.IsUint64() {
		return TransactionPlan{}, errors.New("invalid estimated gas")
	}
	gasPriceRaw, err := c.RPC.CallString(ctx, "eth_gasPrice")
	if err != nil {
		return TransactionPlan{}, err
	}
	gasPrice, err := ParseHexQuantity(gasPriceRaw)
	if err != nil || gasPrice.Sign() <= 0 {
		return TransactionPlan{}, errors.New("invalid gas price")
	}
	return TransactionPlan{To: to, From: from, Data: call["data"], ValueWei: value.String(), GasLimit: gas.Uint64(), GasPriceWei: gasPrice.String()}, nil
}

// Submit signs and broadcasts exactly once; the caller must explicitly opt in.
func (c *Contracts) Submit(ctx context.Context, plan TransactionPlan, key *Key, chainID *big.Int) (ethcommon.Hash, error) {
	if key == nil || key.Address() != plan.From {
		return ethcommon.Hash{}, errors.New("sender does not match private key")
	}
	nonceRaw, err := c.RPC.CallString(ctx, "eth_getTransactionCount", plan.From.Hex(), "pending")
	if err != nil {
		return ethcommon.Hash{}, err
	}
	nonce, err := ParseHexQuantity(nonceRaw)
	if err != nil || !nonce.IsUint64() {
		return ethcommon.Hash{}, errors.New("invalid sender nonce")
	}
	data, err := hex.DecodeString(strings.TrimPrefix(plan.Data, "0x"))
	if err != nil {
		return ethcommon.Hash{}, errors.New("invalid transaction data")
	}
	value, ok := new(big.Int).SetString(plan.ValueWei, 10)
	if !ok {
		return ethcommon.Hash{}, errors.New("invalid transaction value")
	}
	gasPrice, ok := new(big.Int).SetString(plan.GasPriceWei, 10)
	if !ok {
		return ethcommon.Hash{}, errors.New("invalid gas price")
	}
	tx := types.NewTx(&types.LegacyTx{Nonce: nonce.Uint64(), To: &plan.To, Value: value, Gas: plan.GasLimit, GasPrice: gasPrice, Data: data})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(chainID), key.private)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return ethcommon.Hash{}, err
	}
	hash, err := c.RPC.CallString(ctx, "eth_sendRawTransaction", "0x"+hex.EncodeToString(raw))
	if err != nil {
		return ethcommon.Hash{}, err
	}
	if !ethcommon.IsHexHash(hash) {
		return ethcommon.Hash{}, errors.New("invalid transaction hash from RPC")
	}
	return ethcommon.HexToHash(hash), nil
}

// WaitReceipt waits for one confirmed receipt and reports reverted transactions.
func (c *Contracts) WaitReceipt(ctx context.Context, hash ethcommon.Hash) (uint64, error) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		result, err := c.RPC.CallNullable(ctx, "eth_getTransactionReceipt", hash.Hex())
		if err != nil {
			return 0, err
		}
		if string(result) != "null" {
			var receipt struct {
				Status      string `json:"status"`
				BlockNumber string `json:"blockNumber"`
			}
			if err := json.Unmarshal(result, &receipt); err != nil {
				return 0, errors.New("invalid transaction receipt")
			}
			status, err := ParseHexQuantity(receipt.Status)
			if err != nil || !status.IsUint64() || status.Uint64() != 1 {
				return 0, errors.New("transaction reverted")
			}
			block, err := ParseHexQuantity(receipt.BlockNumber)
			if err != nil || !block.IsUint64() {
				return 0, errors.New("invalid receipt block")
			}
			return block.Uint64(), nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}
