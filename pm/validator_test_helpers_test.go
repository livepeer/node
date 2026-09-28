package pm

import (
	"math/big"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

type stubSigVerifier struct{ verifyResult bool }

func (s *stubSigVerifier) SetVerifyResult(valid bool) { s.verifyResult = valid }
func (s *stubSigVerifier) Verify(ethcommon.Address, []byte, []byte) bool {
	return s.verifyResult
}

type stubTimeManager struct{ blkHash [32]byte }

func (*stubTimeManager) LastInitializedRound() *big.Int          { return big.NewInt(0) }
func (s *stubTimeManager) LastInitializedL1BlockHash() [32]byte  { return s.blkHash }
func (*stubTimeManager) PreLastInitializedL1BlockHash() [32]byte { return [32]byte{} }
func (*stubTimeManager) GetTranscoderPoolSize() *big.Int         { return big.NewInt(0) }
func (*stubTimeManager) LastSeenL1Block() *big.Int               { return big.NewInt(0) }
func (*stubTimeManager) SubscribeRounds(chan<- types.Log) event.Subscription {
	return nil
}
func (*stubTimeManager) SubscribeL1Blocks(chan<- *big.Int) event.Subscription {
	return nil
}
