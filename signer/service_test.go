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
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/signercompat"
	"github.com/stretchr/testify/require"
)

type unavailableSender struct{}

func (unavailableSender) ValidateSender(context.Context, ethcommon.Address, *big.Int) error {
	return errors.New("sender has no deposit")
}

func testService(t *testing.T) (*Service, signercompat.OrchestratorInfo) {
	t.Helper()
	private, err := crypto.GenerateKey()
	require.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(crypto.FromECDSA(private))), 0600))
	key, err := eth.OpenKeyFile(keyFile)
	require.NoError(t, err)
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.close() })
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	info := signercompat.OrchestratorInfo{Transcoder: "https://orch.example.com", Address: ethcommon.HexToAddress("0x1234").Bytes(), Price: signercompat.PriceInfo{PricePerUnit: 10, UnitsPerPrice: 1},
		TicketParams: signercompat.TicketParams{Recipient: ethcommon.HexToAddress("0x1234").Bytes(), FaceValue: big.NewInt(10).Bytes(), WinProb: max.Bytes(), RecipientRandHash: crypto.Keccak256(make([]byte, 32)), Seed: big.NewInt(1).Bytes(), ExpirationBlock: big.NewInt(500).Bytes(), Expiration: signercompat.ExpirationParams{CreationRound: 4, CreationRoundBlockHash: ethcommon.HexToHash("0x1234").Bytes()}},
		Auth:         signercompat.AuthToken{Token: []byte("token"), SessionID: "manifest-1", Expiration: time.Now().Add(time.Hour).Unix()}}
	return NewService(key, store, ""), info
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
func TestFixedPaymentAndSignedStateReplay(t *testing.T) {
	s, info := testService(t)
	request := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), "type": "fixed", "ManifestID": "manifest-1"}
	w := postPayment(t, s, request)
	require.Equal(t, 200, w.Code, w.Body.String())
	var first paymentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	decoded, err := base64.StdEncoding.DecodeString(first.Payment)
	require.NoError(t, err)
	payment, err := signercompat.DecodePayment(decoded)
	require.NoError(t, err)
	require.Equal(t, s.key.Address().Bytes(), payment.Sender)
	require.Len(t, payment.SenderParams, 1)
	params := pm.TicketParams{Recipient: ethcommon.BytesToAddress(info.TicketParams.Recipient), FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &pm.TicketExpirationParams{CreationRound: 4, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
	ticket := pm.NewTicket(&params, params.ExpirationParams, s.key.Address(), payment.SenderParams[0].SenderNonce)
	require.True(t, (pm.DefaultSigVerifier{}).Verify(s.key.Address(), ticket.Hash().Bytes(), payment.SenderParams[0].Sig))
	segmentBytes, err := base64.StdEncoding.DecodeString(first.SegCreds)
	require.NoError(t, err)
	segment, err := signercompat.DecodeSegData(segmentBytes)
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
	require.JSONEq(t, w.Body.String(), replay.Body.String())
	request["app"] = "changed"
	w = postPayment(t, s, request)
	require.Equal(t, 400, w.Code)
}

func TestSignerRejectsRemovedTypeAndHighPrice(t *testing.T) {
	s, info := testService(t)
	request := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), "type": "lv2v"}
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
	request := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), "type": "fixed", "ManifestID": "manifest-1"}
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
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestSignerStateFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := openStateStore(path)
	require.NoError(t, err)
	require.NoError(t, store.close())
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.NoError(t, os.Chmod(path, 0644))
	_, err = openStateStore(path)
	require.ErrorContains(t, err, "owner-only")
}
