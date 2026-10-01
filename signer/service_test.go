package signer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

type pipeListener struct {
	conn             net.Conn
	accepted, closed chan struct{}
	once             sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case <-l.accepted:
		<-l.closed
		return nil, net.ErrClosed
	default:
		close(l.accepted)
		return l.conn, nil
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return l.conn.LocalAddr() }

type unavailableSender struct{}

func (unavailableSender) SenderInfo(context.Context, ethcommon.Address, ethcommon.Address) (eth.SenderInfo, error) {
	return eth.SenderInfo{}, errors.New("sender has no deposit")
}

type fundedSender struct{}

func (fundedSender) SenderInfo(context.Context, ethcommon.Address, ethcommon.Address) (eth.SenderInfo, error) {
	return eth.SenderInfo{Snapshot: eth.ChainSnapshot{Block: big.NewInt(50), Round: big.NewInt(5), RoundHash: ethcommon.HexToHash("0x1234")}, Deposit: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil), Reserve: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil), WithdrawRound: new(big.Int)}, nil
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
		TicketParams: wire.TicketParams{Recipient: ethcommon.HexToAddress("0x1234").Bytes(), FaceValue: big.NewInt(20).Bytes(), WinProb: new(big.Int).Add(new(big.Int).Quo(max, big.NewInt(2)), big.NewInt(1)).Bytes(), RecipientRandHash: crypto.Keccak256(make([]byte, 32)), Seed: big.NewInt(1).Bytes(), ExpirationBlock: big.NewInt(500).Bytes(), Expiration: wire.ExpirationParams{CreationRound: 5, CreationRoundBlockHash: ethcommon.HexToHash("0x1234").Bytes()}},
		Auth:         wire.AuthToken{Token: []byte("token"), SessionID: "manifest-1", Expiration: time.Now().Add(time.Hour).Unix()}}
	service := newService(key)
	service.SetPaymentChain(fundedSender{})
	prices, err := newPricePolicy(new(big.Rat).Mul(testRat(t, "1000000000000"), big.NewRat(3600, 1)), testRat(t, "1000000000000"))
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
	params := pm.TicketParams{Recipient: ethcommon.BytesToAddress(info.TicketParams.Recipient), FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &pm.TicketExpirationParams{CreationRound: 5, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
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
	var state paymentState
	require.NoError(t, json.Unmarshal(second.State.State, &state))
	require.Equal(t, uint64(1), state.SequenceNumber)
	require.Equal(t, uint32(2), state.SenderNonce)
	request["state"] = second.State
	request["app"] = "changed"
	w = postPayment(t, s, request)
	require.Equal(t, 400, w.Code)
	delete(request, "app")
	state.Balance = "1000000"
	forged, err := json.Marshal(state)
	require.NoError(t, err)
	request["state"] = signedState{State: forged, Sig: second.State.Sig}
	require.Equal(t, 400, postPayment(t, s, request).Code, "modified credit must fail signature verification")
}

func TestSignerRequestPriceCeiling(t *testing.T) {
	s, info := testService(t)
	request := map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "live"}
	request["maxPrice"] = map[string]any{"price": "9", "currency": "wei", "unit": "seconds"}
	require.Equal(t, 481, postPayment(t, s, request).Code)
	request["maxPrice"] = map[string]any{"price": "10", "currency": "wei", "unit": "seconds"}
	require.Equal(t, 200, postPayment(t, s, request).Code)
}

func TestSignerChecksConfiguredSender(t *testing.T) {
	s, info := testService(t)
	s.SetPaymentChain(unavailableSender{})
	request := map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed", "ManifestID": "manifest-1"}
	require.Equal(t, 482, postPayment(t, s, request).Code)
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
		replicas[i] = newService(key)
		content, err := newPricePolicy(new(big.Rat).Mul(testRat(t, "1000"), big.NewRat(3600, 1)), testRat(t, "1000"))
		require.NoError(t, err)
		require.NoError(t, content.setRate(big.NewRat(1, 1), time.Time{}))
		replicas[i].pricePolicy = content
		replicas[i].SetPaymentChain(fundedSender{})
		require.NoError(t, replicas[i].SetAuthWebhook(testURL(t, webhook.URL), nil))
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

func TestSignerReturnsNoTicketsForCarriedCredit(t *testing.T) {
	s, info := testService(t)
	maxWinProb := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	info.TicketParams.WinProb = new(big.Int).Quo(maxWinProb, big.NewInt(3)).Bytes()
	request := map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed"}
	response := postPayment(t, s, request)
	require.Equal(t, 200, response.Code, response.Body.String())
	var first paymentResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &first))
	// The first batch leaves 10/3 wei of credit. A lower quote and ticket EV
	// can use that credit without asking the signer to create another ticket.
	info.Price.PricePerUnit = 1
	info.TicketParams.FaceValue = big.NewInt(5).Bytes()
	info.TicketParams.RecipientRandHash = crypto.Keccak256([]byte("lower ticket EV"))
	request["orchestrator"], request["state"] = wire.EncodeOrchestratorInfo(info), first.State
	response = postPayment(t, s, request)
	require.Equal(t, 482, response.Code, response.Body.String())
	require.NotContains(t, response.Body.String(), `"payment"`)
}

func TestSignerDiscoveryPreservesLiveRunnerEntries(t *testing.T) {
	s, _ := testService(t)
	orchestrator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/discovery", r.URL.Path)
		_, _ = w.Write([]byte(`[{"address":"https://orchestrator.example","runners":[{"url":"https://orchestrator.example/apps/r/app","app":"retained","mode":"single-shot","gpu":{"id":"0","name":"H100","vram_mb":80000},"capacity":2,"price_info":{"price":10,"currency":"wei","unit":"fixed"}},{"url":"https://orchestrator.example/apps/x/app","app":"excluded","mode":"single-shot","capacity":1}]}]`))
	}))
	defer orchestrator.Close()
	require.NoError(t, s.SetDiscovery([]*url.URL{testURL(t, orchestrator.URL)}))
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

func TestSlowResponseReleasesSlotOnDeadlineOrCancellation(t *testing.T) {
	for _, earlyCancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", earlyCancel), func(t *testing.T) {
			s, info := testService(t)
			s.slots = make(chan struct{}, 1)
			info.Auth.SessionID = strings.Repeat("m", 32<<10) // Force a write beyond the HTTP buffer.
			body, err := json.Marshal(map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed"})
			require.NoError(t, err)
			serverConn, clientConn := net.Pipe()
			defer clientConn.Close()
			listener := &pipeListener{conn: serverConn, accepted: make(chan struct{}), closed: make(chan struct{})}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			finished := make(chan struct{})
			server := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(finished); s.ServeHTTP(w, r) })}
			defer server.Close()
			go func() { _ = server.Serve(listener) }()
			require.NoError(t, clientConn.SetWriteDeadline(time.Now().Add(3*time.Second)))
			_, err = fmt.Fprintf(clientConn, "POST /generate-live-payment HTTP/1.1\r\nHost: local\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			require.NoError(t, err)
			require.Eventually(t, func() bool { return len(s.slots) == 1 }, 500*time.Millisecond, time.Millisecond)
			if earlyCancel {
				cancel()
			}
			select {
			case <-finished:
				response := httptest.NewRecorder()
				s.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/sign-orchestrator-info", nil))
				require.Equal(t, http.StatusOK, response.Code, "the next request must be admitted after the blocked response ends")
			case <-time.After(2 * time.Second):
				t.Fatal("slow reader pinned the signer after its context ended")
			}
		})
	}
}

func TestSignerRefreshResetsNonceAtBatchLimit(t *testing.T) {
	s, info := testService(t)
	info.Price.PricePerUnit = 1000
	req := map[string]any{"type": "fixed", "ManifestID": "manifest-1", "orchestrator": wire.EncodeOrchestratorInfo(info)}
	var state paymentState
	for range 5 {
		w := postPayment(t, s, req)
		require.Equal(t, 200, w.Code, w.Body.String())
		var res paymentResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &res))
		require.NoError(t, json.Unmarshal(res.State.State, &state))
		req["state"] = res.State
	}
	require.Equal(t, uint32(500), state.SenderNonce)
	require.Equal(t, 480, postPayment(t, s, req).Code, "exhausted parameters must request a refresh")
	info.TicketParams.RecipientRandHash = crypto.Keccak256([]byte("fresh recipient randomness"))
	req["orchestrator"] = wire.EncodeOrchestratorInfo(info)
	w := postPayment(t, s, req)
	require.Equal(t, 200, w.Code, "new recipient-random hash should reset the nonce; got %s", w.Body.String())
	var refreshed paymentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &refreshed))
	require.NoError(t, json.Unmarshal(refreshed.State.State, &state))
	require.Equal(t, uint32(100), state.SenderNonce)
	require.Equal(t, uint64(5), state.SequenceNumber)
}

func TestSignerEnforcesTicketExposureLimit(t *testing.T) {
	s, info := testService(t)
	info.Price.PricePerUnit = 1
	info.TicketParams.FaceValue = big.NewInt(1_000_000_000_000_000_000).Bytes()
	req := map[string]any{"type": "fixed", "ManifestID": "manifest-1", "orchestrator": wire.EncodeOrchestratorInfo(info), "maxPrice": map[string]any{"price": "1", "currency": "wei", "unit": "fixed"}}
	w := postPayment(t, s, req)
	require.Equal(t, 400, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "ticket expected value exceeds sender policy")
}

func TestPaymentParamsValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*wire.OrchestratorInfo)
		status int
	}{
		{"valid", func(*wire.OrchestratorInfo) {}, 200},
		{"auth expiry", func(i *wire.OrchestratorInfo) { i.Auth.Expiration = time.Now().Unix() }, 480},
		{"parameter expiry", func(i *wire.OrchestratorInfo) { i.TicketParams.ExpirationBlock = []byte{51} }, 480},
		{"older round", func(i *wire.OrchestratorInfo) { i.TicketParams.Expiration.CreationRound-- }, 480},
		{"future round", func(i *wire.OrchestratorInfo) { i.TicketParams.Expiration.CreationRound++ }, 480},
		{"zero hash", func(i *wire.OrchestratorInfo) { i.TicketParams.Expiration.CreationRoundBlockHash = make([]byte, 32) }, 480},
		{"wrong hash", func(i *wire.OrchestratorInfo) {
			i.TicketParams.Expiration.CreationRoundBlockHash = ethcommon.HexToHash("0xabcd").Bytes()
		}, 480},
		{"recipient mismatch", func(i *wire.OrchestratorInfo) { i.TicketParams.Recipient = ethcommon.HexToAddress("0xabcd").Bytes() }, 400},
		{"zero recipient", func(i *wire.OrchestratorInfo) { i.Address = make([]byte, 20); i.TicketParams.Recipient = i.Address }, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, info := testService(t)
			tc.change(&info)
			w := postPayment(t, s, map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(wire.EncodeOrchestratorInfo(info)), "type": "fixed"})
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == 480 {
				require.Equal(t, info.Transcoder, w.Header().Get("Livepeer-Orchestrator-URL"))
			}
		})
	}
}
