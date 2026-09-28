package pm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
)

// TimeManager is the retained round and L1 block interface from pm/broker.go.
type TimeManager interface {
	LastInitializedRound() *big.Int
	LastInitializedL1BlockHash() [32]byte
	PreLastInitializedL1BlockHash() [32]byte
	GetTranscoderPoolSize() *big.Int
	LastSeenL1Block() *big.Int
	SubscribeRounds(chan<- types.Log) event.Subscription
	SubscribeL1Blocks(chan<- *big.Int) event.Subscription
}
