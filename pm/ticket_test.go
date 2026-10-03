package pm

import (
	"encoding/hex"
	"math"
	"math/big"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestTicketExpectedValue(t *testing.T) {
	for _, tt := range []struct {
		name     string
		face     int64
		prob     *big.Int
		ev, odds string
	}{
		{"half", 1000, new(big.Int).Div(maxWinProb, big.NewInt(2)), "500", "0.50"},
		{"quarter", 1000, new(big.Int).Div(maxWinProb, big.NewInt(4)), "250", "0.25"},
		{"zero odds", 1000, new(big.Int), "0", "0.00"},
		{"maximum odds", 1000, maxWinProb, "1000", "1.00"},
		{"zero value", 0, big.NewInt(999), "0", "0.00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ticket := &Ticket{FaceValue: big.NewInt(tt.face), WinProb: tt.prob}
			require.Equal(t, tt.ev, ticket.EV().FloatString(0))
			require.Equal(t, tt.odds, ticket.WinProbRat().FloatString(2))
		})
	}
}

func TestAuxData(t *testing.T) {
	hash := ethcommon.HexToHash("7624778dedc75f8b322b9fa1632a610d40b85e106c7d9bf0e743a9ce291b9c6f")
	for _, tt := range []struct {
		name  string
		round int64
		hash  ethcommon.Hash
		want  string
	}{
		{"zero round", 0, hash, "00000000000000000000000000000000000000000000000000000000000000007624778dedc75f8b322b9fa1632a610d40b85e106c7d9bf0e743a9ce291b9c6f"},
		{"zero hash", 5, ethcommon.Hash{}, "00000000000000000000000000000000000000000000000000000000000000050000000000000000000000000000000000000000000000000000000000000000"},
		{"no expiration", 0, ethcommon.Hash{}, ""},
		{"round and hash", 5, hash, "00000000000000000000000000000000000000000000000000000000000000057624778dedc75f8b322b9fa1632a610d40b85e106c7d9bf0e743a9ce291b9c6f"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ticket := &Ticket{CreationRound: tt.round, CreationRoundBlockHash: tt.hash}
			require.Equal(t, tt.want, hex.EncodeToString(ticket.AuxData()))
		})
	}
}

func TestHash(t *testing.T) {
	for _, tt := range []struct {
		name   string
		ticket Ticket
		hash   string
	}{
		{"maximum nonce", Ticket{TicketNonce: math.MaxUint32},
			"e1393fc7f6de093780674022f96cb8e3872235167d037c04d554e58c0e63d280"},
		{"nonce", Ticket{TicketNonce: 1},
			"ce0918ba94518293e9712effbe5fca4f1f431089833a5b8c257cb1e024595f68"},
		{"probability", Ticket{FaceValue: big.NewInt(1), WinProb: big.NewInt(500)},
			"87ef13f2d37e4d5352a01d3c77b8179d80e0887f1953bfabfd0bfd7b0f689ddd"},
		{"face value", Ticket{FaceValue: big.NewInt(500), WinProb: big.NewInt(1)},
			"aa4b35071043587992ac8b9dd1b2cf1d8311130e6458cf0b2342484e21af5f5b"},
		{"recipient", Ticket{Recipient: ethcommon.HexToAddress("73AEd7b5dEb30222fa896f399d46cC99c7BEe57F")},
			"81e353ae39046e2b77b9f996abbaeed527fda559b61ef90f299c4499dd508ccc"},
		{"commitment", Ticket{RecipientRandHash: ethcommon.HexToHash("41b1a0649752af1b28b3dc29a1556eee781e4a4c3a1f7f53f90fa834de098c4d")},
			"be5eaf9a49a39540b9bb02f1a21904f61324a29819e2c032824bfaa10c100b17"},
		{"expiration", Ticket{CreationRound: 10, CreationRoundBlockHash: ethcommon.HexToHash("41b1a0649752af1b28b3dc29a1556eee781e4a4c3a1f7f53f90fa834de098c4d")},
			"e502907c16036ab3d11b78c5a2e93c20f3d6415c67f91de4ed01348182b3b2e2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.ticket.FaceValue == nil {
				tt.ticket.FaceValue = new(big.Int)
			}
			if tt.ticket.WinProb == nil {
				tt.ticket.WinProb = new(big.Int)
			}
			require.Equal(t, ethcommon.HexToHash(tt.hash), tt.ticket.Hash())
		})
	}
}

func TestTickets(t *testing.T) {
	batch := &TicketBatch{
		TicketParams:           &TicketParams{Recipient: RandAddress(), FaceValue: big.NewInt(100), WinProb: big.NewInt(500), RecipientRandHash: RandHash()},
		TicketExpirationParams: &TicketExpirationParams{CreationRound: 10, CreationRoundBlockHash: RandHash()},
		PayerAddress:           RandAddress(),
	}
	for _, size := range []int{0, 1, 2} {
		batch.PayerParams = make([]*TicketPayerParams, size)
		for i := range size {
			batch.PayerParams[i] = &TicketPayerParams{TicketNonce: uint32(i)}
		}
		tickets := batch.Tickets()
		require.Len(t, tickets, size)
		for i, ticket := range tickets {
			require.Equal(t, &Ticket{Recipient: batch.Recipient, PayerAddress: batch.PayerAddress,
				FaceValue: batch.FaceValue, WinProb: batch.WinProb, TicketNonce: uint32(i),
				RecipientRandHash: batch.RecipientRandHash, CreationRound: batch.CreationRound,
				CreationRoundBlockHash: batch.CreationRoundBlockHash}, ticket)
		}
	}
}
