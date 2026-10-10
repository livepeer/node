package chain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/nodeconfig"
)

func signFile(p OperatorParams, d DisplayOptions, out io.Writer, path string, typed bool) error {
	key, err := eth.OpenAccount(filepath.Join(p.DataDir, "keystore"), nodeconfig.Path(p.DataDir, p.KeystoreFile), p.KeystorePassword, p.KeystorePasswordFile, p.Account)
	if err != nil {
		return fmt.Errorf("open signing keystore (check --keystore-file or --data-dir and --account, and LIVEPEER_CHAIN_KEYSTORE_PASSWORD or --keystore-password-file): %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("signing input file is unavailable (check --message-file for 'sign message' or --data-file for 'sign typed-data'): %w", err)
	}
	var hash []byte
	if typed {
		var value apitypes.TypedData
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("invalid typed data JSON in --data-file: %w", err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return fmt.Errorf("invalid typed data JSON in --data-file: %w", err)
		}
		for _, name := range []string{"types", "domain", "message"} {
			raw := bytes.TrimSpace(fields[name])
			if len(raw) == 0 || raw[0] != '{' {
				return fmt.Errorf("typed data %s in --data-file must be an object", name)
			}
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return errors.New("--data-file must contain one typed data JSON object")
		}
		exact, conversionErr := exactTypedValues(value.Message)
		if conversionErr != nil {
			return fmt.Errorf("invalid --data-file: %w", conversionErr)
		}
		value.Message = exact.(map[string]any)
		hash, err = typedDataHash(value)
		if err != nil {
			return fmt.Errorf("invalid typed data in --data-file: %w", err)
		}
	} else {
		hash = accounts.TextHash(data)
	}
	signature, err := key.SignHash(hash)
	if err != nil {
		return err
	}
	return formatResult(out, d.Output, map[string]any{"address": key.Address().Hex(), "hash": hexutil.Encode(hash), "signature": eth.HexSignature(signature)})
}

func exactTypedValues(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		integer, ok := new(big.Int).SetString(string(v), 10)
		if !ok {
			return nil, errors.New("typed-data numbers must be integers")
		}
		return integer, nil
	case map[string]any:
		for name, item := range v {
			converted, err := exactTypedValues(item)
			if err != nil {
				return nil, err
			}
			v[name] = converted
		}
		return v, nil
	case []any:
		for i, item := range v {
			converted, err := exactTypedValues(item)
			if err != nil {
				return nil, err
			}
			v[i] = converted
		}
		return v, nil
	default:
		return value, nil
	}
}

func typedDataHash(value apitypes.TypedData) ([]byte, error) {
	if value.PrimaryType == "" {
		return nil, errors.New("primaryType is required")
	}
	if _, ok := value.Types[value.PrimaryType]; !ok {
		return nil, errors.New("primaryType is undefined")
	}
	if _, ok := value.Types["EIP712Domain"]; !ok {
		return nil, errors.New("EIP712Domain type is required")
	}
	for _, fields := range value.Types {
		seen := map[string]bool{}
		for _, field := range fields {
			if seen[field.Name] {
				return nil, errors.New("duplicate typed-data field")
			}
			seen[field.Name] = true
		}
	}
	domain := value.Domain.Map()
	// Geth requires a nonempty domain in its validator. EIP-712 also permits
	// EIP712Domain(); keep the actual encoded domain empty in that case.
	if len(domain) == 0 && len(value.Types["EIP712Domain"]) == 0 {
		value.Domain.Name = "validation"
	}
	separator, err := value.HashStruct("EIP712Domain", domain)
	if err != nil {
		return nil, err
	}
	message, err := value.HashStruct(value.PrimaryType, value.Message)
	if err != nil {
		return nil, err
	}
	return crypto.Keccak256([]byte{0x19, 0x01}, separator, message), nil
}
