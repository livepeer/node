package signer

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/signercompat"
	"github.com/stretchr/testify/require"
)

func TestRegressionSignerAcceptsRefreshedParamsAfter500Nonces(t *testing.T) {
	s, info := testService(t)
	info.Price.PricePerUnit = 1000
	req := map[string]any{"type": "fixed", "ManifestID": "manifest-1", "orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info))}
	for range 5 {
		w := postPayment(t, s, req)
		require.Equal(t, 200, w.Code, w.Body.String())
		var res paymentResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
		req["state"] = res.State
	}
	info.TicketParams.RecipientRandHash = crypto.Keccak256([]byte("fresh recipient randomness"))
	req["orchestrator"] = base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info))
	w := postPayment(t, s, req)
	require.Equal(t, 200, w.Code, "new recipient-random hash should reset the nonce; got %s", w.Body.String())
}
func TestRegressionSignerLimitsTicketExposure(t *testing.T) {
	s, info := testService(t)
	info.Price.PricePerUnit = 1
	info.TicketParams.FaceValue = big.NewInt(1_000_000_000_000_000_000).Bytes()
	req := map[string]any{"type": "fixed", "ManifestID": "manifest-1", "orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), "maxPrice": map[string]any{"price": "1", "currency": "wei", "unit": "fixed"}}
	w := postPayment(t, s, req)
	if w.Code == 200 {
		var res paymentResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
		b, err := base64.StdEncoding.DecodeString(res.Payment)
		require.NoError(t, err)
		p, err := signercompat.DecodePayment(b)
		require.NoError(t, err)
		t.Errorf("1-wei quote signed %d effectively certain winning ticket(s) with face value %s wei", len(p.SenderParams), new(big.Int).SetBytes(p.TicketParams.FaceValue))
	}
}
