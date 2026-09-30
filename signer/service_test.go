package signer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/pm/wire"
	"github.com/stretchr/testify/require"
)

type unavailableSender struct{}

func (unavailableSender) SenderInfo(context.Context, ethcommon.Address, ethcommon.Address) (eth.SenderInfo, error) {
	return eth.SenderInfo{}, errors.New("sender has no deposit")
}

type fundedSender struct{}

func (fundedSender) SenderInfo(context.Context, ethcommon.Address, ethcommon.Address) (eth.SenderInfo, error) {
	return eth.SenderInfo{Snapshot: eth.ChainSnapshot{Block: big.NewInt(50), Round: big.NewInt(5)}, Deposit: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil), Reserve: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil), WithdrawRound: new(big.Int)}, nil
}

func testService(t *testing.T) (*Service, wire.OrchestratorInfo) {
	t.Helper()
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	key, err := eth.OpenKeyFile(keyFile)
	require.NoError(t, err)
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	info := wire.OrchestratorInfo{Transcoder: "https://orch.example.com", Address: ethcommon.HexToAddress("0x1234").Bytes(), Price: wire.PriceInfo{PricePerUnit: 10, UnitsPerPrice: 1},
		TicketParams: wire.TicketParams{Recipient: ethcommon.HexToAddress("0x1234").Bytes(), FaceValue: big.NewInt(20).Bytes(), WinProb: new(big.Int).Add(new(big.Int).Quo(max, big.NewInt(2)), big.NewInt(1)).Bytes(), RecipientRandHash: crypto.Keccak256(make([]byte, 32)), Seed: big.NewInt(1).Bytes(), ExpirationBlock: big.NewInt(500).Bytes(), Expiration: wire.ExpirationParams{CreationRound: 4, CreationRoundBlockHash: ethcommon.HexToHash("0x1234").Bytes()}},
		Auth:         wire.AuthToken{Token: []byte("token"), SessionID: "manifest-1", Expiration: time.Now().Add(time.Hour).Unix()}}
	service := newService(key, "")
	service.SetPaymentChain(fundedSender{})
	prices, err := newPricePolicy("1000000000000", "1000000000000")
	require.NoError(t, err)
	require.NoError(t, prices.setRate(big.NewRat(1, 1), time.Time{}))
	service.pricePolicy = prices
	return service, info
}

func postPayment(t *testing.T, s *Service, request map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/generate-live-payment", bytes.NewReader(body)))
	return w
}

// Adapted from go-livepeer/server/remote_signer_test.go's fixed/live payment,
// signed-state, price ceiling and unsupported-type cases.
func TestFixedPaymentAndSignedStateContinuation(t *testing.T) {
	s, info := testService(t)
	request := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(wire.EncodeOrchestratorInfo(info)), "type": "fixed", "ManifestID": "manifest-1"}
	w := postPayment(t, s, request)
	require.Equal(t, 200, w.Code, w.Body.String())
	var first paymentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	decoded, err := base64.StdEncoding.DecodeString(first.Payment)
	require.NoError(t, err)
	payment, err := wire.DecodePayment(decoded)
	require.NoError(t, err)
	require.Equal(t, s.key.Address().Bytes(), payment.Sender)
	require.Len(t, payment.SenderParams, 1)
	params := pm.TicketParams{Recipient: ethcommon.BytesToAddress(info.TicketParams.Recipient), FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &pm.TicketExpirationParams{CreationRound: 4, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
	ticket := pm.NewTicket(&params, params.ExpirationParams, s.key.Address(), payment.SenderParams[0].SenderNonce)
	require.True(t, (pm.DefaultSigVerifier{}).Verify(s.key.Address(), ticket.Hash().Bytes(), payment.SenderParams[0].Sig))
	segmentBytes, err := base64.StdEncoding.DecodeString(first.SegCreds)
	require.NoError(t, err)
	segment, err := wire.DecodeSegData(segmentBytes)
	require.NoError(t, err)
	require.Equal(t, "manifest-1", string(segment.ManifestID))
	request["state"] = first.State
	w = postPayment(t, s, request)
	require.Equal(t, 200, w.Code, w.Body.String())
	var second paymentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &second))
	require.NotEqual(t, first.State.State, second.State.State)
	replay := postPayment(t, s, request)
	require.Equal(t, 200, replay.Code, replay.Body.String())
	var replayed paymentResponse
	require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &replayed))
	require.True(t, (pm.DefaultSigVerifier{}).Verify(s.key.Address(), replayed.State.State, replayed.State.Sig))
	request["app"] = "changed"
	w = postPayment(t, s, request)
	require.Equal(t, 400, w.Code)
}

func TestSignerRejectsRemovedTypeAndHighPrice(t *testing.T) {
	s, info := testService(t)
	request := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(wire.EncodeOrchestratorInfo(info)), "type": "lv2v"}
	require.Equal(t, 400, postPayment(t, s, request).Code)
	request["type"] = "live"
	request["maxPrice"] = map[string]any{"price": "9", "currency": "wei", "unit": "seconds"}
	require.Equal(t, 481, postPayment(t, s, request).Code)
	request["maxPrice"] = map[string]any{"price": "10", "currency": "wei", "unit": "seconds"}
	require.Equal(t, 200, postPayment(t, s, request).Code)
}

func TestSignerChecksConfiguredSenderAndAuth(t *testing.T) {
	s, info := testService(t)
	s.authToken = "secret"
	s.SetPaymentChain(unavailableSender{})
	request := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(wire.EncodeOrchestratorInfo(info)), "type": "fixed", "ManifestID": "manifest-1"}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/generate-live-payment", bytes.NewReader(body)))
	require.Equal(t, http.StatusUnauthorized, w.Code)
	r := httptest.NewRequest(http.MethodPost, "/generate-live-payment", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	require.Equal(t, 482, w.Code)
}

func TestSignedStateMovesBetweenIndependentSigners(t *testing.T) {
	_, info := testService(t)
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "shared-key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	var calls atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct{ State paymentState }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.NotEmpty(t, body.State.Balance)
		require.NotZero(t, body.State.SenderNonce)
		require.False(t, body.State.LastUpdate.IsZero())
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "expiry": time.Now().Add(time.Hour).Unix(), "auth_id": "replica-user"})
	}))
	defer webhook.Close()
	var replicas [2]*Service
	for i := range replicas {
		key, err := eth.OpenKeyFile(keyFile)
		require.NoError(t, err)
		replicas[i] = newService(key, "")
		content, err := newPricePolicy("1000", "1000")
		require.NoError(t, err)
		require.NoError(t, content.setRate(big.NewRat(1, 1), time.Time{}))
		replicas[i].pricePolicy = content
		replicas[i].SetPaymentChain(fundedSender{})
		require.NoError(t, replicas[i].SetAuthWebhook(webhook.URL, []string{strings.TrimPrefix(webhook.URL, "http://")}, "", nil))
	}
	for _, kind := range []string{"fixed", "live"} {
		t.Run(kind, func(t *testing.T) {
			request := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(wire.EncodeOrchestratorInfo(info)), "type": kind}
			var previous paymentState
			for i := range 3 {
				if i == 2 {
					refreshed := info
					refreshed.TicketParams.RecipientRandHash = crypto.Keccak256([]byte(kind + "-refreshed"))
					request["orchestrator"] = base64.StdEncoding.EncodeToString(wire.EncodeOrchestratorInfo(refreshed))
				}
				response := postPayment(t, replicas[i%2], request)
				require.Equal(t, 200, response.Code, response.Body.String())
				var payment paymentResponse
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payment))
				require.NotEmpty(t, payment.Payment)
				var state paymentState
				require.NoError(t, json.Unmarshal(payment.State.State, &state))
				require.Equal(t, uint64(i), state.SequenceNumber)
				require.Equal(t, "replica-user", state.AuthID)
				if i > 0 {
					require.Equal(t, previous.StateID, state.StateID)
				}
				if i == 2 {
					require.NotEqual(t, previous.PMSessionID, state.PMSessionID)
				}
				previous = state
				request["state"] = payment.State
			}
		})
	}
	require.Equal(t, int32(2), calls.Load(), "webhook authorization is cached in signed state across replicas")
}

func TestSignerReturns482AtTicketEVFloor(t *testing.T) {
	s, info := testService(t)
	request := map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed"}
	response := postPayment(t, s, request)
	require.Equal(t, 200, response.Code, response.Body.String())
	var first paymentResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &first))
	var state paymentState
	require.NoError(t, json.Unmarshal(first.State.State, &state))
	maxWinProb := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	ev := new(big.Rat).SetFrac(new(big.Int).Mul(new(big.Int).SetBytes(info.TicketParams.FaceValue), new(big.Int).SetBytes(info.TicketParams.WinProb)), maxWinProb)
	state.Balance = ev.RatString()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	sig, err := s.key.SignMessage(data)
	require.NoError(t, err)
	request["state"] = signedState{State: data, Sig: sig}
	response = postPayment(t, s, request)
	require.Equal(t, 482, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), "payment")
}

func TestSignerDiscoveryPreservesLiveRunnerEntries(t *testing.T) {
	s, _ := testService(t)
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/discovery", r.URL.Path)
		_, _ = w.Write([]byte(`[{"address":"https://orchestrator.example","runners":[{"url":"https://orchestrator.example/apps/r/app","app":"retained","mode":"single-shot","gpu":{"id":"0","name":"H100","vram_mb":80000},"capacity":2,"price_info":{"price":10,"currency":"wei","unit":"fixed"}},{"url":"https://orchestrator.example/apps/x/app","app":"excluded","mode":"single-shot","capacity":1}]}]`))
	}))
	defer orchestrator.Close()
	require.NoError(t, s.SetDiscovery([]string{orchestrator.URL}, []string{strings.TrimPrefix(orchestrator.URL, "http://")}, ""))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discover-orchestrators?app=retained&gpu=H100", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var result []discoveredOrchestrator
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Len(t, result, 1)
	require.Equal(t, "https://orchestrator.example", result[0].Address)
	require.Len(t, result[0].Runners, 1)
	require.Equal(t, "retained", result[0].Runners[0].App)
	require.Equal(t, "H100", result[0].Runners[0].GPU.Name)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/discover-orchestrators?app=retained&gpu=L40S", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, "[]", w.Body.String())
}
