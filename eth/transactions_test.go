package eth

import (
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

func TestTransactionFeesAndNonceReservations(t *testing.T) {
	keyFile, passwordPath := test.WriteFixedKeystore(t)
	key, err := OpenKeystoreFile(keyFile, passwordPath)
	require.NoError(t, err)
	var nonceReads, sends atomic.Int32
	var missingBaseFee atomic.Bool
	rpc := testRPC(t, func(method string, params []json.RawMessage) any {
		var result any
		switch method {
		case "eth_call":
			result = "0x"
		case "eth_estimateGas":
			result = "0x5208"
		case "eth_getBlockByNumber":
			header := &types.Header{Number: big.NewInt(1), Difficulty: new(big.Int)}
			if !missingBaseFee.Load() {
				header.BaseFee = big.NewInt(100)
			}
			result = header
		case "eth_maxPriorityFeePerGas":
			require.False(t, missingBaseFee.Load())
			result = "0x3"
		case "eth_getTransactionCount":
			nonceReads.Add(1)
			result = "0x7"
		case "eth_sendRawTransaction":
			sends.Add(1)
			// Deliberately disagree with the locally saved signed hash.
			result = common.HexToHash("0xffff").Hex()
		default:
			t.Fatalf("unexpected RPC %s", method)
		}
		return result
	})
	c, err := NewContracts(rpc, common.HexToAddress("0x1000"))
	require.NoError(t, err)
	var plan TransactionPlan
	for _, tc := range []struct {
		name           string
		missingBaseFee bool
		ceiling        int64
		wantErr        string
	}{
		{"dynamic fees", false, 0, ""},
		{"fee at ceiling", false, 203, ""},
		{"fee exceeds ceiling", false, 200, "exceeds maximum"},
		{"missing base fee", true, 0, "missing base fee"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missingBaseFee.Store(tc.missingBaseFee)
			c.MaxFeePerGas = nil
			if tc.ceiling > 0 {
				c.MaxFeePerGas = big.NewInt(tc.ceiling)
			}
			candidate, err := c.PlanTransaction(t.Context(), key.Address(), common.HexToAddress("0x2000"), []byte{1, 2}, big.NewInt(5))
			require.Zero(t, nonceReads.Load(), "planning must not reserve a nonce")
			require.Zero(t, sends.Load(), "planning must not broadcast")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "203", candidate.FeeCapWei)
			require.Equal(t, "3", candidate.TipCapWei)
			plan = candidate
		})
	}

	// Fee boundaries share the same nonce and persistence behavior; check it once.
	missingBaseFee.Store(false)
	c.MaxFeePerGas = big.NewInt(203)
	c.SetNonceFloor(key.Address(), 40)
	saveErr := errors.New("storage unavailable")
	_, err = c.PrepareAndStore(t.Context(), plan, key, big.NewInt(42161), func(tx SignedTransaction) error {
		require.EqualValues(t, 40, tx.Nonce)
		return saveErr
	})
	require.ErrorIs(t, err, saveErr)
	require.Zero(t, sends.Load(), "failed persistence must never broadcast")
	var wg sync.WaitGroup
	prepared := make(chan SignedTransaction, 3)
	for range 3 {
		wg.Go(func() {
			tx, err := c.PrepareAndStore(t.Context(), plan, key, big.NewInt(42161), func(SignedTransaction) error { return nil })
			require.NoError(t, err)
			prepared <- tx
		})
	}
	wg.Wait()
	close(prepared)
	var nonces []int
	for signed := range prepared {
		var tx types.Transaction
		require.NoError(t, tx.UnmarshalBinary(signed.Raw))
		require.Equal(t, uint8(types.DynamicFeeTxType), tx.Type())
		require.Equal(t, big.NewInt(203), tx.GasFeeCap())
		require.Equal(t, big.NewInt(3), tx.GasTipCap())
		require.Equal(t, big.NewInt(42161), tx.ChainId())
		require.Equal(t, big.NewInt(5), tx.Value())
		require.Equal(t, []byte{1, 2}, tx.Data())
		sender, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
		require.NoError(t, err)
		require.Equal(t, key.Address(), sender)
		nonces = append(nonces, int(tx.Nonce()))
		if len(nonces) == 1 {
			require.ErrorContains(t, c.Broadcast(t.Context(), signed), "does not match signed transaction")
		}
	}
	sort.Ints(nonces)
	require.Equal(t, []int{40, 41, 42}, nonces, "failed saves must not skip a nonce; concurrent saved identities must reserve distinct nonces above saved history")
	require.EqualValues(t, 1, sends.Load())
	// The ceiling is checked again before signing an externally supplied plan.
	c.MaxFeePerGas = big.NewInt(202)
	_, err = c.Prepare(t.Context(), plan, key, big.NewInt(42161))
	require.ErrorContains(t, err, "exceeds maximum")
}
