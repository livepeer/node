package pm

import (
	ethcommon "github.com/ethereum/go-ethereum/common"
	"math/rand"
)

func RandHash() ethcommon.Hash {
	return ethcommon.BytesToHash(RandBytes(32))
}

func RandAddress() ethcommon.Address {
	return ethcommon.BytesToAddress(RandBytes(addressSize))
}

func RandBytes(size uint) []byte {
	x := make([]byte, size, size)
	for i := 0; i < len(x); i++ {
		x[i] = byte(rand.Uint32())
	}
	return x
}
