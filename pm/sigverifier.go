// Adapted from livepeer/go-livepeer/pm/sigverifier.go and crypto/verify.go,
// originally by Yondon Fu and Elad Mallel; Michael Ira Krufky moved VerifySig
// to crypto. The 27/28 recovery convention follows Yondon Fu's upstream commit
// 3b52399fc7b9177460864207a68eb9f3d2f4ee57.
package pm

import (
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// SigVerifier verifies Ethereum signed-message signatures for ticket payers.
type SigVerifier interface {
	Verify(addr ethcommon.Address, msg, sig []byte) bool
}

type DefaultSigVerifier struct{}

func (DefaultSigVerifier) Verify(addr ethcommon.Address, msg, sig []byte) bool {
	recovered, err := recoverTicketSigner(msg, sig)
	return err == nil && recovered == addr
}

var secp256k1HalfN = new(big.Int).Div(
	new(big.Int).Set(crypto.S256().Params().N), big.NewInt(2),
)

// recoverTicketSigner preserves the legacy 27/28 V convention and rejects
// malleable high-S signatures before attempting Ethereum signed-message recovery.
func recoverTicketSigner(msg, sig []byte) (ethcommon.Address, error) {
	if len(sig) != 65 {
		return ethcommon.Address{}, errors.New("invalid signature length")
	}
	if new(big.Int).SetBytes(sig[32:64]).Cmp(secp256k1HalfN) > 0 {
		return ethcommon.Address{}, errors.New("signature s value too high")
	}
	if sig[64] != 27 && sig[64] != 28 {
		return ethcommon.Address{}, errors.New("signature v value must be 27 or 28")
	}
	converted := append([]byte(nil), sig...)
	converted[64] -= 27
	pubkey, err := crypto.SigToPub(accounts.TextHash(msg), converted)
	if err != nil {
		return ethcommon.Address{}, err
	}
	return crypto.PubkeyToAddress(*pubkey), nil
}
