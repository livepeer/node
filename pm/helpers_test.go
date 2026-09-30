package pm

import (
	"crypto/rand"

	ethcommon "github.com/ethereum/go-ethereum/common"
)

// RandHash returns a random keccak256 hash
func RandHash() ethcommon.Hash {
	return ethcommon.BytesToHash(RandBytes(32))
}

// RandAddress returns a random ETH address
func RandAddress() ethcommon.Address {
	return ethcommon.BytesToAddress(RandBytes(addressSize))
}

// RandBytes returns a slice of random bytes with the size specified by the caller
func RandBytes(size uint) []byte {
	x := make([]byte, size)
	if _, err := rand.Read(x); err != nil {
		panic("cryptographic randomness unavailable")
	}
	return x
}
