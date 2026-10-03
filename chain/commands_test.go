package chain

import (
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
)

// Construct expected ABI bytes independently of the production action builders.
func calldata(signature string, words ...string) string {
	return "0x" + hex.EncodeToString(crypto.Keccak256([]byte(signature))[:4]) + strings.Join(words, "")
}

func uintWord(value uint64) string { return fmt.Sprintf("%064x", value) }
func addressWord(address string) string {
	return strings.Repeat("0", 24) + strings.ToLower(address[2:])
}
