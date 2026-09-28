package eth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

// PaymentChain adapts the retained Livepeer contracts to pm's narrow chain
// interface. Ethereum contract reads and redemption stay in eth.
type PaymentChain struct{ Contracts *Contracts }

func (c PaymentChain) IsActive(ctx context.Context, recipient ethcommon.Address) (bool, error) {
	address, err := c.Contracts.Resolve(ctx, "bondingManager")
	if err != nil {
		return false, err
	}
	result, err := c.Contracts.Call(ctx, "bondingManager", address, "isActiveTranscoder", recipient)
	if err != nil {
		return false, err
	}
	active, ok := result[0].(bool)
	if !ok {
		return false, errors.New("invalid orchestrator active status")
	}
	return active, nil
}

// Receipt reports a submitted redemption without retrying or resubmitting it.
func (c PaymentChain) Receipt(ctx context.Context, hash ethcommon.Hash) (confirmed, reverted bool, err error) {
	data, err := c.Contracts.RPC.CallNullable(ctx, "eth_getTransactionReceipt", hash.Hex())
	if err != nil {
		return false, false, err
	}
	if string(data) == "null" {
		return false, false, nil
	}
	var receipt struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return false, false, errors.New("invalid redemption receipt")
	}
	status, err := ParseHexQuantity(receipt.Status)
	if err != nil || !status.IsUint64() {
		return false, false, errors.New("invalid redemption receipt status")
	}
	if status.Uint64() == 1 {
		return true, false, nil
	}
	if status.Uint64() == 0 {
		return false, true, nil
	}
	return false, false, errors.New("invalid redemption receipt status")
}

type ChainSnapshot struct {
	Block     *big.Int
	Round     *big.Int
	RoundHash ethcommon.Hash
}

func (c PaymentChain) Snapshot(ctx context.Context) (ChainSnapshot, error) {
	blockRaw, err := c.Contracts.RPC.CallString(ctx, "eth_blockNumber")
	if err != nil {
		return ChainSnapshot{}, err
	}
	block, err := ParseHexQuantity(blockRaw)
	if err != nil {
		return ChainSnapshot{}, err
	}
	address, err := c.Contracts.Resolve(ctx, "roundsManager")
	if err != nil {
		return ChainSnapshot{}, err
	}
	roundResult, err := c.Contracts.Call(ctx, "roundsManager", address, "currentRound")
	if err != nil {
		return ChainSnapshot{}, err
	}
	round, ok := roundResult[0].(*big.Int)
	if !ok || !round.IsInt64() {
		return ChainSnapshot{}, errors.New("invalid current round")
	}
	hashResult, err := c.Contracts.Call(ctx, "roundsManager", address, "blockHashForRound", round)
	if err != nil {
		return ChainSnapshot{}, err
	}
	hash, ok := hashResult[0].([32]byte)
	if !ok {
		return ChainSnapshot{}, errors.New("invalid round block hash")
	}
	return ChainSnapshot{Block: block, Round: round, RoundHash: ethcommon.BytesToHash(hash[:])}, nil
}

func tupleBigInt(value any, field string) (*big.Int, error) {
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, errors.New("invalid contract tuple")
	}
	f := v.FieldByName(field)
	if !f.IsValid() {
		return nil, fmt.Errorf("missing contract tuple field %s", field)
	}
	result, ok := f.Interface().(*big.Int)
	if !ok || result == nil {
		return nil, errors.New("invalid contract integer")
	}
	return result, nil
}

func (c PaymentChain) ValidateSender(ctx context.Context, sender ethcommon.Address, faceValue *big.Int) error {
	address, err := c.Contracts.Resolve(ctx, "ticketBroker")
	if err != nil {
		return err
	}
	values, err := c.Contracts.Call(ctx, "ticketBroker", address, "getSenderInfo", sender)
	if err != nil {
		return err
	}
	if len(values) != 2 {
		return errors.New("invalid sender info")
	}
	deposit, err := tupleBigInt(values[0], "Deposit")
	if err != nil {
		return err
	}
	reserve, err := tupleBigInt(values[1], "FundsRemaining")
	if err != nil {
		return err
	}
	if deposit.Cmp(faceValue) < 0 || reserve.Sign() <= 0 {
		return errors.New("sender deposit or reserve is insufficient")
	}
	return nil
}

type brokerTicket struct {
	Recipient         ethcommon.Address
	Sender            ethcommon.Address
	FaceValue         *big.Int
	WinProb           *big.Int
	SenderNonce       *big.Int
	RecipientRandHash [32]byte
	AuxData           []byte
}

type RedeemTicket struct {
	Recipient         ethcommon.Address
	Sender            ethcommon.Address
	FaceValue         *big.Int
	WinProb           *big.Int
	SenderNonce       uint32
	RecipientRandHash ethcommon.Hash
	AuxData           []byte
	Signature         []byte
	RecipientRand     *big.Int
}

func (c PaymentChain) Redeem(ctx context.Context, key *Key, chainID *big.Int, t RedeemTicket) (ethcommon.Hash, error) {
	if key == nil || key.Address() != t.Recipient || t.FaceValue == nil || t.WinProb == nil || t.RecipientRand == nil {
		return ethcommon.Hash{}, errors.New("redemption recipient key mismatch")
	}
	address, err := c.Contracts.Resolve(ctx, "ticketBroker")
	if err != nil {
		return ethcommon.Hash{}, err
	}
	coreTicket := brokerTicket{Recipient: t.Recipient, Sender: t.Sender, FaceValue: t.FaceValue, WinProb: t.WinProb, SenderNonce: new(big.Int).SetUint64(uint64(t.SenderNonce)), RecipientRandHash: t.RecipientRandHash, AuxData: t.AuxData}
	data, err := c.Contracts.Pack("ticketBroker", "redeemWinningTicket", coreTicket, t.Signature, t.RecipientRand)
	if err != nil {
		return ethcommon.Hash{}, err
	}
	plan, err := c.Contracts.PlanTransaction(ctx, key.Address(), address, data, big.NewInt(0))
	if err != nil {
		return ethcommon.Hash{}, err
	}
	return c.Contracts.Submit(ctx, plan, key, chainID)
}
