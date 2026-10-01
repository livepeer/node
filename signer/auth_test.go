package signer

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livepeer/node/pm/wire"
	"github.com/stretchr/testify/require"
)

func TestAuthWebhookCachePriceAndIdentity(t *testing.T) {
	s, info := testService(t)
	var calls atomic.Int32
	var reject atomic.Bool
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "configured", r.Header.Get("X-Webhook-Secret"))
		status := 200
		if reject.Load() {
			status = 403
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "reason": "denied", "expiry": time.Now().Add(time.Minute).Unix(), "auth_id": "alice", "maxPrice": map[string]any{"price": "10", "currency": " WEI ", "unit": " FIXED "}})
	}))
	defer webhook.Close()
	require.NoError(t, s.SetAuthWebhook(testURL(t, webhook.URL), Headers{"X-Webhook-Secret": {"configured"}}))
	req := map[string]any{"orchestrator": base64.StdEncoding.EncodeToString(wire.EncodeOrchestratorInfo(info)), "type": "fixed"}
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
	state.AuthExpiry = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, s.authorizePayment(httptest.NewRequest("POST", "/generate-live-payment", nil), paymentRequest{Type: "fixed"}, info.Price, &state))
	require.Equal(t, int32(2), calls.Load(), "expired authorization must call the webhook again")
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
	require.NoError(t, s.SetAuthWebhook(testURL(t, webhook.URL), Headers{"X-Webhook-Secret": {"configured"}, "X-Policy": {"new"}}))
	w = postPayment(t, s, req)
	require.Equal(t, 403, w.Code, w.Body.String())
	require.Equal(t, int32(3), calls.Load())
	require.NotContains(t, w.Body.String(), `"payment"`)
	require.NotContains(t, w.Body.String(), `"state"`)
}

func TestAuthWebhookFailuresWithholdPayment(t *testing.T) {
	tests := map[string]struct {
		body         string
		status, want int
	}{
		"/http-error": {"", 500, 502}, "/redirect": {"", 302, 502},
		"/malformed": {"not JSON", 200, 502}, "/oversized": {strings.Repeat(" ", (1<<20)+1), 200, 502},
		"/missing-status": {`{}`, 200, 502}, "/invalid-status": {`{"status":201}`, 200, 502},
		"/price-limit": {`{"status":200,"maxPrice":{"price":9,"currency":"wei","unit":"fixed"}}`, 200, 481},
	}
	var calls atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		test := tests[r.URL.Path]
		w.Header().Set("Location", "/redirect")
		w.WriteHeader(test.status)
		_, _ = io.WriteString(w, test.body)
	}))
	defer webhook.Close()
	s, info := testService(t)
	defer s.Close()
	request := map[string]any{"orchestrator": wire.EncodeOrchestratorInfo(info), "type": "fixed"}
	for path, test := range tests {
		t.Run(path, func(t *testing.T) {
			before := calls.Load()
			require.NoError(t, s.SetAuthWebhook(testURL(t, webhook.URL+path), nil))
			requirePaymentFailure(t, postPayment(t, s, request), test.want)
			if test.status == 302 {
				require.Equal(t, before+1, calls.Load(), "authorization redirects must not be followed")
			}
		})
	}
	webhook.Close()
	require.NoError(t, s.SetAuthWebhook(testURL(t, webhook.URL), nil))
	requirePaymentFailure(t, postPayment(t, s, request), 502)
}
