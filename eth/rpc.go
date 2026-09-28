package eth

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/livepeer/node/destination"
)

const maxRPCResponse = 1 << 20

// RPC is a bounded JSON-RPC client with a purpose-specific destination policy.
// Each connection is resolved and checked by destination.Policy at dial time.
type RPC struct {
	endpoint string
	client   *http.Client
}

func OpenRPC(endpoint string, grants []string, caFile string) (*RPC, error) {
	if _, err := destination.ValidateURL(endpoint); err != nil {
		return nil, errors.New("invalid Ethereum RPC URL")
	}
	policy, err := destination.New("ethereum-rpc", grants)
	if err != nil {
		return nil, err
	}
	policy, err = policy.WithCAFile(caFile)
	if err != nil {
		return nil, err
	}
	client := policy.Client()
	client.Timeout = 15 * time.Second
	return &RPC{endpoint: endpoint, client: client}, nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func (r *RPC) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	result, err := r.CallNullable(ctx, method, params...)
	if err != nil {
		return nil, err
	}
	if string(result) == "null" {
		return nil, errors.New("empty Ethereum RPC result")
	}
	return result, nil
}

func (r *RPC) CallNullable(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	data, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("invalid Ethereum RPC endpoint")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return nil, errors.New("ethereum RPC request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ethereum RPC returned HTTP %d", response.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, maxRPCResponse+1))
	if err != nil || len(data) > maxRPCResponse {
		return nil, errors.New("ethereum RPC response exceeds 1 MiB")
	}
	var decoded rpcResponse
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.JSONRPC != "2.0" || decoded.ID != 1 {
		return nil, errors.New("invalid Ethereum RPC response")
	}
	if decoded.Error != nil {
		return nil, fmt.Errorf("ethereum RPC error %d", decoded.Error.Code)
	}
	if len(decoded.Result) == 0 {
		return nil, errors.New("empty Ethereum RPC result")
	}
	return decoded.Result, nil
}

func (r *RPC) CallString(ctx context.Context, method string, params ...any) (string, error) {
	data, err := r.Call(ctx, method, params...)
	if err != nil {
		return "", err
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", errors.New("invalid Ethereum RPC string result")
	}
	return value, nil
}

func ParseHexQuantity(raw string) (*big.Int, error) {
	if !strings.HasPrefix(raw, "0x") || len(raw) < 3 {
		return nil, errors.New("invalid Ethereum RPC quantity")
	}
	value, ok := new(big.Int).SetString(raw[2:], 16)
	if !ok || value.Sign() < 0 {
		return nil, errors.New("invalid Ethereum RPC quantity")
	}
	return value, nil
}

func ValidAddress(address string) bool {
	if len(address) != 42 || !strings.HasPrefix(address, "0x") {
		return false
	}
	_, err := hex.DecodeString(address[2:])
	return err == nil
}

func (r *RPC) CheckChainID(ctx context.Context, expected *big.Int) error {
	raw, err := r.CallString(ctx, "eth_chainId")
	if err != nil {
		return err
	}
	id, err := ParseHexQuantity(raw)
	if err != nil || id.Cmp(expected) != 0 {
		return errors.New("RPC chain ID does not match configured chain-id")
	}
	return nil
}
