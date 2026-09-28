package eth

import (
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/accounts"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Key is a local Ethereum signer. The key material never crosses the JSON-RPC
// boundary and is not included in command output or errors.
type Key struct{ private *ecdsa.PrivateKey }

func OpenKeyFile(path string) (*Key, error) {
	if path == "" {
		return nil, errors.New("private-key-file is required for submission")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, errors.New("private key file is unavailable")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private key file must be owner-only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("private key file is unavailable")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(string(data)), "0x"))
	if err != nil {
		return nil, errors.New("invalid private key file")
	}
	return &Key{private: key}, nil
}

func (k *Key) Address() ethcommon.Address { return crypto.PubkeyToAddress(k.private.PublicKey) }

func (k *Key) SignMessage(message []byte) ([]byte, error) {
	sig, err := crypto.Sign(accounts.TextHash(message), k.private)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

func (k *Key) SignHash(hash []byte) ([]byte, error) {
	if len(hash) != 32 {
		return nil, errors.New("ethereum signing hash must be 32 bytes")
	}
	sig, err := crypto.Sign(hash, k.private)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

func HexSignature(sig []byte) string { return "0x" + hex.EncodeToString(sig) }
