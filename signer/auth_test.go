package signer

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livepeer/node/signercompat"
	"github.com/stretchr/testify/require"
)

func TestAuthWebhookCachePriceAndIdentity(t *testing.T) {
	s, info := testService(t)
	var calls atomic.Int32
	var reject atomic.Bool
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "configured", r.Header.Get("X-Webhook-Secret"))
		var body struct {
			Headers http.Header
			State   paymentState
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "manifest-1", body.State.ManifestID)
		if calls.Load() == 1 {
			require.Equal(t, uint64(0), body.State.SequenceNumber)
			require.Equal(t, uint32(1), body.State.SenderNonce)
		}
		require.NotEmpty(t, body.State.PMSessionID)
		require.NotEmpty(t, body.State.Balance)
		require.WithinDuration(t, time.Now(), body.State.LastUpdate, time.Second)
		if calls.Load() == 1 {
			require.Empty(t, body.State.AuthID)
		}
		status := 200
		if reject.Load() {
			status = 403
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "reason": "denied", "expiry": time.Now().Add(time.Minute).Unix(), "auth_id": "alice", "maxPrice": map[string]any{"price": "10", "currency": " WEI ", "unit": " FIXED "}})
	}))
	defer webhook.Close()
	require.NoError(t, s.SetAuthWebhook(webhook.URL, []string{strings.TrimPrefix(webhook.URL, "http://")}, "", map[string]string{"X-Webhook-Secret": "configured"}))
	req := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), "type": "fixed"}
	w := postPayment(t, s, req)
	require.Equal(t, 200, w.Code, w.Body.String())
	var first paymentResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
	var state paymentState
	require.NoError(t, json.Unmarshal(first.State.State, &state))
	require.Equal(t, "alice", state.AuthID)
	require.Equal(t, "10", state.AuthMaxPrice)
	req["state"] = first.State
	w = postPayment(t, s, req)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, int32(1), calls.Load())
	// Identity checks apply to signed state on any replica.
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest("POST", "/generate-live-payment", strings.NewReader(string(body)))
	r.Header.Set("Signer-Auth-Id", "bob")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	require.Equal(t, 403, w.Code)
	// Changing configured credentials invalidates prior webhook authorization.
	var second paymentResponse
	require.NoError(t, json.Unmarshal(postPayment(t, s, req).Body.Bytes(), &second))
	req["state"] = second.State
	reject.Store(true)
	require.NoError(t, s.SetAuthWebhook(webhook.URL, []string{strings.TrimPrefix(webhook.URL, "http://")}, "", map[string]string{"X-Webhook-Secret": "configured", "X-Policy": "new"}))
	w = postPayment(t, s, req)
	require.Equal(t, 403, w.Code, w.Body.String())
	require.Equal(t, int32(2), calls.Load())
	require.NotContains(t, w.Body.String(), "payment")
	require.NotContains(t, w.Body.String(), "state")
}

func TestAuthWebhookPriceRejectsBeforeResponse(t *testing.T) {
	s, info := testService(t)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":200,"maxPrice":{"price":9,"currency":"wei","unit":"fixed"}}`))
	}))
	defer webhook.Close()
	require.NoError(t, s.SetAuthWebhook(webhook.URL, []string{strings.TrimPrefix(webhook.URL, "http://")}, "", nil))
	w := postPayment(t, s, map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), "type": "fixed"})
	require.Equal(t, 481, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "segCreds")
	require.NotContains(t, w.Body.String(), "payment")
	require.NotContains(t, w.Body.String(), "state")
}

func TestExpiredParamsRequestRefresh(t *testing.T) {
	for _, expired := range []string{"auth", "params"} {
		t.Run(expired, func(t *testing.T) {
			s, info := testService(t)
			if expired == "auth" {
				info.Auth.Expiration = time.Now().Add(-time.Second).Unix()
			} else {
				info.TicketParams.ExpirationBlock = []byte{51}
			}
			w := postPayment(t, s, map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(signercompat.EncodeOrchestratorInfo(info)), "type": "fixed"})
			require.Equal(t, 480, w.Code, w.Body.String())
			require.Equal(t, info.Transcoder, w.Header().Get("Livepeer-Orchestrator-URL"))
		})
	}
}
