package redeemer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/observability"
	"github.com/livepeer/node/internal/test"
	"github.com/livepeer/node/nodeconfig"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/redemption"
	"github.com/stretchr/testify/require"
)

func testAuthWebhook(t *testing.T, endpoint string, headers nodeconfig.Headers, logs io.Writer) *authWebhook {
	t.Helper()
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	auth := newAuthWebhook(u, headers, slog.New(slog.NewTextHandler(logs, nil)), observability.New("redeemer"))
	t.Cleanup(auth.client.CloseIdleConnections)
	return auth
}

func TestAuthWebhookHeadersAndRequests(t *testing.T) {
	type webhookRequest struct {
		Headers http.Header     `json:"headers"`
		Ticket  json.RawMessage `json:"ticket"`
	}
	received := make(chan webhookRequest, 1)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "POST", r.Method)
		require.Equal(t, "token=private-url", r.URL.RawQuery)
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.Equal(t, "Bearer service-secret", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Values("X-Caller"))
		var payload webhookRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		require.NoError(t, decoder.Decode(&payload))
		received <- payload
		_, _ = io.WriteString(w, `{"status":200,"expiry":9999999999,"auth_id":"ignored","maxPrice":{}}`)
	}))
	defer server.Close()
	headers := nodeconfig.Headers{"Authorization": {"Bearer service-secret"}, "Content-Type": {"text/plain"}}
	auth := testAuthWebhook(t, server.URL+"?token=private-url", headers, io.Discard)
	headers["Authorization"][0] = "mutated"
	require.Equal(t, 5*time.Second, auth.client.Timeout)
	store, err := redemption.OpenRedeemerDB(filepath.Join(t.TempDir(), "tickets.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	binding := redemption.Binding{ChainID: "1", Controller: common.HexToAddress("0x1000"), Role: "redeemer", Destination: "chain"}
	require.NoError(t, store.Bind(t.Context(), binding, redemption.DefaultLimits, nil))
	ticket := redemption.TicketJSON{
		Payer: common.HexToAddress("0x1234"), Recipient: common.HexToAddress("0x5678"),
		FaceValue: "1000", WinProb: "42", Nonce: 7,
		RandHash: common.HexToHash("0x1234"), CreationRound: "1", RoundHash: common.HexToHash("0x5678"),
		Expiration: "10", Signature: []byte{1, 2, 3}, Randomness: "9",
	}
	body, err := json.Marshal(redemption.TicketRequest{ChainID: binding.ChainID, Controller: binding.Controller, Ticket: ticket})
	require.NoError(t, err)
	handler := redemption.HandlerWithAuthorization(store, auth.authorize)
	for _, tc := range []struct {
		body   string
		ticket *redemption.TicketJSON
	}{
		{"bad ticket", nil},
		{strings.Repeat(" ", redemption.MaxTicketBody) + string(body), nil},
		{string(body), &ticket},
	} {
		r := httptest.NewRequest("POST", "/tickets", strings.NewReader(tc.body))
		r.Header = http.Header{"Authorization": {"Bearer caller-secret"}, "X-Caller": {"one", "two"}}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		require.Equal(t, 400, w.Code, "approval does not bypass ticket decoding or validation")
		payload := <-received
		require.Equal(t, r.Header, payload.Headers)
		if tc.ticket == nil {
			require.JSONEq(t, "null", string(payload.Ticket))
			continue
		}
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(payload.Ticket, &fields))
		require.NotContains(t, fields, "recipient_rand", "the secret field must be absent, not empty or null")
		require.Contains(t, fields, "sig")
		require.Contains(t, fields, "recipient_rand_hash")
		var actual redemption.TicketJSON
		require.NoError(t, json.Unmarshal(payload.Ticket, &actual))
		expected := *tc.ticket
		expected.Randomness = ""
		require.Equal(t, expected, actual, "all other ticket fields reach the webhook unchanged")
	}
	for _, target := range []string{"/healthz", "/readyz", "/metrics", "/other"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", target, nil))
	}
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/tickets", nil))
	require.EqualValues(t, 3, calls.Load(), "approvals are never cached")
}

func TestAuthWebhookDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		upstream, want int
		outcome        string
	}{
		{"approval", `{"status":200}`, 200, 200, "success"},
		{"nullable reason", `{"status":200,"reason":null}`, 200, 200, "success"},
		{"denial", `{"status":403,"reason":"private-detail"}`, 200, 403, "denied"},
		{"conflict denial", `{"status":409}`, 200, 403, "denied"},
		{"service denial", `{"status":503}`, 200, 503, "denied"},
		{"last denial", `{"status":599}`, 200, 599, "denied"},
		{"non-200", "private-detail", 500, 502, "error"},
		{"redirect", "private-detail", 307, 502, "error"},
		{"malformed", "private-detail", 200, 502, "error"},
		{"trailing JSON", `{"status":200}{}`, 200, 502, "error"},
		{"missing status", `{}`, 200, 502, "error"},
		{"null status", `{"status":null}`, 200, 502, "error"},
		{"string status", `{"status":"200"}`, 200, 502, "error"},
		{"fractional status", `{"status":200.5}`, 200, 502, "error"},
		{"invalid status", `{"status":201}`, 200, 502, "error"},
		{"invalid denial", `{"status":600}`, 200, 502, "error"},
		{"invalid reason", `{"status":200,"reason":123}`, 200, 502, "error"},
		{"oversized", strings.Repeat(" ", (1<<20)+1), 200, 502, "error"},
		{"read failure", `{"status":200}`, 200, 502, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "/redirected")
				if tc.name == "read failure" {
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+10))
				}
				w.WriteHeader(tc.upstream)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			var logs bytes.Buffer
			auth := testAuthWebhook(t, server.URL+"?private-detail", nodeconfig.Headers{"Authorization": {"private-detail"}}, &logs)
			r := httptest.NewRequest("POST", "/tickets", nil)
			w := httptest.NewRecorder()
			allowed := auth.authorize(w, r, nil)
			require.Equal(t, tc.want == 200, allowed)
			require.Equal(t, tc.want, w.Code)
			require.EqualValues(t, 1, calls.Load())
			require.NotContains(t, logs.String(), "private-detail")
			require.NotContains(t, w.Body.String(), "private-detail")
			require.EqualValues(t, 1, test.MetricSum(t, auth.metrics.Registry, "redeemer_auth_webhook_requests_total", map[string]string{"status": strconv.Itoa(tc.upstream), "outcome": tc.outcome}))
			require.EqualValues(t, 1, test.MetricSum(t, auth.metrics.Registry, "redeemer_auth_webhook_duration_seconds", nil))
		})
	}
}

func TestAuthWebhookUnavailableTimeoutAndCancellation(t *testing.T) {
	for _, kind := range []string{"unavailable", "timeout", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}))
			defer server.Close()
			if kind == "unavailable" {
				server.Close()
			}
			var logs bytes.Buffer
			auth := testAuthWebhook(t, server.URL+"?private-detail", nil, &logs)
			if kind == "timeout" {
				auth.client.Timeout = 20 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			r := httptest.NewRequest("POST", "/tickets", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			require.False(t, auth.authorize(w, r, nil))
			require.Equal(t, 502, w.Code)
			require.NotContains(t, logs.String(), "private-detail")
			require.EqualValues(t, 1, test.MetricSum(t, auth.metrics.Registry, "redeemer_auth_webhook_requests_total", map[string]string{"status": "0", "outcome": "error"}))
		})
	}
}

func TestAuthWebhookDoesNotReplayReceivedPOST(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close() // Lose the response on an already pooled connection.
			return
		}
		_, _ = io.WriteString(w, `{"status":200}`)
	}))
	defer server.Close()
	auth := testAuthWebhook(t, server.URL, nodeconfig.Headers{"Idempotency-Key": {"configured"}}, io.Discard)
	require.True(t, auth.authorize(httptest.NewRecorder(), httptest.NewRequest("POST", "/tickets", nil), nil))
	w := httptest.NewRecorder()
	require.False(t, auth.authorize(w, httptest.NewRequest("POST", "/tickets", nil), nil))
	require.Equal(t, 502, w.Code)
	require.EqualValues(t, 2, calls.Load(), "the received second POST must not be replayed")
}

func TestAuthorizationForwardingAndQueuedRedemption(t *testing.T) {
	f := test.NewRedemptionRPC(t)
	p := serviceParams(t, f)
	rpc, err := eth.NewRPC(p.RPCURL, nil)
	require.NoError(t, err)
	defer rpc.Close()
	contracts, err := eth.NewContracts(rpc, *p.Controller)
	require.NoError(t, err)
	contracts.CheckRedemptionCost = true
	chain := eth.PaymentChain{Contracts: contracts}
	contracts.BeforeBroadcast = redemption.StandalonePolicy(chain, big.NewInt(1))
	newStore := func(role, target string) (*redemption.RedeemerDB, *sql.DB) {
		db, err := redemption.OpenDatabase(t.Context(), filepath.Join(t.TempDir(), "tickets.sqlite"), true)
		require.NoError(t, err)
		s, err := redemption.NewStore(db)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, s.Close()) })
		require.NoError(t, s.Bind(t.Context(), redemption.Binding{ChainID: "1", Controller: *p.Controller, Role: role, Destination: target}, redemption.Limits{Pending: 1, PerRecipient: 1}, chain))
		return s, db
	}
	serverStore, serverDB := newStore("redeemer", "chain")
	var decision, calls atomic.Int32
	decision.Store(409)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload struct {
			Ticket redemption.TicketJSON `json:"ticket"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Empty(t, payload.Ticket.Randomness)
		// Signature verification needs only the commitment, not its preimage.
		payload.Ticket.Randomness = "0"
		unsigned, err := payload.Ticket.SignedTicket()
		require.NoError(t, err)
		require.True(t, (pm.DefaultSigVerifier{}).Verify(unsigned.PayerAddress, unsigned.Hash().Bytes(), unsigned.Sig))
		_, _ = io.WriteString(w, `{"status":`+big.NewInt(int64(decision.Load())).String()+`}`)
	}))
	defer webhook.Close()
	auth := testAuthWebhook(t, webhook.URL, nil, io.Discard)
	server := httptest.NewServer(redemption.HandlerWithAuthorization(serverStore, auth.authorize))
	defer server.Close()
	clientStore, clientDB := newStore("orchestrator", server.URL)
	policy, err := destination.New("redeemer", []string{"127.0.0.1"})
	require.NoError(t, err)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	remote, err := redemption.NewHTTPDestination(u, policy, redemption.Binding{ChainID: "1", Controller: *p.Controller})
	require.NoError(t, err)
	defer remote.Close()
	payer, err := crypto.GenerateKey()
	require.NoError(t, err)
	random := big.NewInt(1)
	ticket := &pm.SignedTicket{Ticket: &pm.Ticket{PayerAddress: crypto.PubkeyToAddress(payer.PublicKey), Recipient: common.HexToAddress("0x5678"), FaceValue: big.NewInt(1e12), WinProb: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), TicketNonce: 1, RecipientRandHash: crypto.Keccak256Hash(common.LeftPadBytes(random.Bytes(), 32)), CreationRound: 1, CreationRoundBlockHash: common.HexToHash("0x1"), ParamsExpirationBlock: big.NewInt(10)}, RecipientRand: random}
	ticket.Sig, err = crypto.Sign(accounts.TextHash(ticket.Hash().Bytes()), payer)
	require.NoError(t, err)
	ticket.Sig[64] += 27
	f.Mu.Lock()
	f.Tickets[ticket.Hash()] = ticket.RecipientRandHash
	f.Finalized = false
	f.Mu.Unlock()
	require.NoError(t, clientStore.Admit(t.Context(), ticket))
	forwarder, err := redemption.NewWorker(clientStore, chain, remote)
	require.NoError(t, err)
	require.NotEmpty(t, forwarder.Process(t.Context()))
	var delivery string
	require.NoError(t, clientDB.QueryRow("SELECT delivery FROM ticket_state").Scan(&delivery))
	require.Equal(t, "retryable", delivery, "webhook 409 must not permanently reject delivery")
	var count int
	require.NoError(t, serverDB.QueryRow("SELECT count(*) FROM winning_tickets").Scan(&count))
	require.Zero(t, count, "denied requests do not write ticket state")
	decision.Store(200)
	_, err = clientDB.Exec("UPDATE ticket_state SET next_attempt=0")
	require.NoError(t, err)
	require.Empty(t, forwarder.Process(t.Context()))
	require.NoError(t, remote.Submit(t.Context(), ticket), "approved duplicates succeed at queue capacity")
	ticket.ParamsExpirationBlock = big.NewInt(11)
	err = remote.Submit(t.Context(), ticket)
	var rejection *redemption.Rejection
	require.ErrorAs(t, err, &rejection, "actual scheduling conflicts remain permanent")
	ticket.ParamsExpirationBlock = big.NewInt(10)
	decision.Store(403)
	err = remote.Submit(t.Context(), ticket)
	require.Error(t, err, "duplicates are authorized again")
	require.False(t, errors.As(err, &rejection))
	require.EqualValues(t, 5, calls.Load())
	webhook.Close()
	require.Error(t, remote.Submit(t.Context(), ticket), "outage withholds duplicate acknowledgment")
	key, err := eth.OpenKeystoreFile(p.KeystoreFile, p.KeystorePasswordFile)
	require.NoError(t, err)
	chainDest, err := redemption.NewChainDestination(t.Context(), serverStore, chain, key, big.NewInt(1), redemption.RedemptionOptions{Timeout: 30 * time.Millisecond, MaxReplacements: 1})
	require.NoError(t, err)
	worker, err := redemption.NewWorker(serverStore, chain, chainDest)
	require.NoError(t, err)
	require.Empty(t, worker.Process(t.Context()))
	require.Eventually(t, func() bool { return len(f.Transactions()) == 2 }, 3*time.Second, 5*time.Millisecond)
	sent := f.Transactions()
	require.Equal(t, sent[0].Nonce(), sent[1].Nonce())
	require.Equal(t, sent[0].Data(), sent[1].Data(), "transaction replacement proceeds during authorization outage")
	require.EqualValues(t, 5, calls.Load(), "background redemption never authorizes again")
}
