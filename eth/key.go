package eth

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// Key is a local Ethereum signer. The key material never crosses the JSON-RPC
// boundary and is not included in command output or errors.
type Key struct{ private *ecdsa.PrivateKey }

// OpenKeystoreFile unlocks one encrypted geth account file. Password files use
// exact bytes, including whitespace; neither input is included in diagnostics.
func OpenKeystoreFile(keystorePath, passwordPath string) (*Key, error) {
	if keystorePath == "" || passwordPath == "" {
		return nil, errors.New("keystore-file and keystore-password-file are required")
	}
	return OpenKeystore(keystorePath, nil, passwordPath)
}

// OpenKeystore unlocks an encrypted geth account using password, or passwordPath
// if password is nil. Any supplied password file must be owner-only.
func OpenKeystore(keystorePath string, password *string, passwordPath string) (key *Key, err error) {
	if keystorePath == "" || password == nil && passwordPath == "" {
		return nil, errors.New("keystore-file and keystore-password or keystore-password-file are required")
	}
	data, err := readOwnerOnlyFile(keystorePath, "keystore file")
	if err != nil {
		return nil, err
	}
	if password == nil {
		contents, err := readOwnerOnlyFile(passwordPath, "keystore password file")
		if err != nil {
			return nil, err
		}
		defer clear(contents)
		password = new(string(contents))
	} else if passwordPath != "" {
		file, err := openOwnerOnlyFile(passwordPath, "keystore password file")
		if err != nil {
			return nil, err
		}
		file.Close()
	}
	// Geth's KDF decoder can panic on malformed JSON parameters. Sanitize both
	// panics and returned decryption errors without exposing either input.
	defer func() {
		if recover() != nil || err != nil {
			key, err = nil, errors.New("cannot decrypt keystore: invalid file or password")
		}
	}()
	decoded, err := keystore.DecryptKey(data, *password)
	if err != nil {
		return nil, err
	}
	return &Key{private: decoded.PrivateKey}, nil
}

func readOwnerOnlyFile(path, label string) ([]byte, error) {
	file, err := openOwnerOnlyFile(path, label)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("%s is unavailable", label)
	}
	return data, nil
}

func openOwnerOnlyFile(path, label string) (*os.File, error) {
	// Reject devices and FIFOs before opening; check the opened handle as well
	// so permissions and type checks apply to the file actually read.
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s is unavailable", label)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%s is unavailable", label)
	}
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("%s is unavailable", label)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	if info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, fmt.Errorf("%s must be owner-only", label)
	}
	return file, nil
}

func (k *Key) Address() ethcommon.Address { return crypto.PubkeyToAddress(k.private.PublicKey) }

// Ethereum message signing and the 27/28 convention follow Yondon Fu's
// go-livepeer/eth/accountmanager.go (3b52399fc7b9177460864207a68eb9f3d2f4ee57).
func (k *Key) SignMessage(message []byte) ([]byte, error) {
	return k.SignHash(accounts.TextHash(message))
}

// Hash signing follows Yondon Fu's go-livepeer/eth/accountmanager.go signHash
// (97a014cb692a736a853f7ebb396e9cbada5408a6).
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

func HexSignature(sig []byte) string { return hexutil.Encode(sig) }
