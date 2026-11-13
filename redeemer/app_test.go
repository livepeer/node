package redeemer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/observability"
	"github.com/livepeer/node/internal/test"
	"github.com/livepeer/node/nodeconfig"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/redemption"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func bindAddress(t *testing.T) boa.HTTPBind {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	var bind boa.HTTPBind
	require.NoError(t, bind.UnmarshalText([]byte(address)))
	return bind
}

func serviceParams(t *testing.T, f *test.RedemptionRPC) Params {
	t.Helper()
	file, password := test.WriteFixedKeystore(t)
	key, err := eth.OpenKeystoreFile(file, password)
	require.NoError(t, err)
	f.Mu.Lock()
	f.Wallet = key.Address()
	f.Mu.Unlock()
	rpc, err := url.Parse(f.Server.URL)
	require.NoError(t, err)
	id := uint64(1)
	controller := common.HexToAddress("0x1000")
	return Params{Settings: nodeconfig.Settings{Network: "test", DataDir: t.TempDir()}, Listen: bindAddress(t), MetricsListen: bindAddress(t), RedeemerDB: filepath.Join(t.TempDir(), "tickets.sqlite"), KeystoreFile: file, KeystorePasswordFile: password, RPCURL: rpc, ChainID: &id, Controller: &controller, RedeemerTxTimeout: time.Second, RedeemerMaxTxReplacements: 1, MaxPendingTickets: 32, MaxTicketsPerRecipient: 8}
}

func TestDedicatedWalletStartupPolicy(t *testing.T) {
	for _, kind := range []string{"registered", "active", "unavailable", "wrong-chain"} {
		t.Run(kind, func(t *testing.T) {
			f := test.NewRedemptionRPC(t)
			p := serviceParams(t, f)
			f.Mu.Lock()
			switch kind {
			case "registered":
				f.Registered = true
			case "active":
				f.WalletActive = true
			case "unavailable":
				f.SnapshotError = true
			case "wrong-chain":
				*p.ChainID = 2
			}
			f.Mu.Unlock()
			require.Error(t, Serve(t.Context(), p, io.Discard))
			require.NoFileExists(t, p.RedeemerDB, "wallet and chain checks precede database creation")
		})
	}
}

func TestServeProbesAndShutdown(t *testing.T) {
	for _, tc := range []struct{ busy, auth bool }{{}, {busy: true}, {auth: true}} {
		t.Run(fmt.Sprintf("busy=%t/auth=%t", tc.busy, tc.auth), func(t *testing.T) {
			f := test.NewRedemptionRPC(t)
			p := serviceParams(t, f)
			var calls atomic.Int32
			if tc.auth {
				webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.WriteString(w, `{"status":403}`)
				}))
				defer webhook.Close()
				p.AuthWebhook, _ = url.Parse(webhook.URL)
			}
			if tc.busy {
				seedBusyQueue(t, p)
				handler := f.Server.Config.Handler
				selector := fmt.Sprintf("0x%x", crypto.Keccak256([]byte("usedTickets(bytes32)"))[:4])
				f.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					var request struct {
						Method string
						Params []json.RawMessage
					}
					require.NoError(t, json.Unmarshal(body, &request))
					if request.Method == "eth_call" {
						var call struct{ Input string }
						require.NoError(t, json.Unmarshal(request.Params[0], &call))
						if strings.HasPrefix(call.Input, selector) {
							<-r.Context().Done()
							return
						}
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					handler.ServeHTTP(w, r)
				})
			}
			wait := 5 * time.Second
			if tc.busy {
				wait = 40 * time.Second
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, p, io.Discard) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(3 * time.Second):
					t.Error("redeemer did not stop")
				}
			})
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			ready := func() bool {
				response, err := client.Get("http://" + p.MetricsListen.String() + "/readyz")
				if err != nil {
					return false
				}
				defer response.Body.Close()
				return response.StatusCode == 200
			}
			require.Eventually(t, ready, wait, 10*time.Millisecond)
			response, err := client.Get("http://" + p.MetricsListen.String() + "/metrics")
			require.NoError(t, err)
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Contains(t, string(data), `component="redeemer"`)
			response, err = client.Get("http://" + p.Listen.String() + "/tickets")
			require.NoError(t, err)
			response.Body.Close()
			require.Equal(t, 405, response.StatusCode)
			if tc.auth {
				require.Zero(t, calls.Load(), "probes and other methods do not authorize")
				response, err = client.Post("http://"+p.Listen.String()+"/tickets", "application/json", strings.NewReader("invalid ticket"))
				require.NoError(t, err)
				response.Body.Close()
				require.Equal(t, 403, response.StatusCode, "invalid submissions are still authorized")
				require.True(t, ready(), "authorization denial does not change readiness")
				require.EqualValues(t, 1, calls.Load())
			}
			info, err := os.Stat(p.RedeemerDB)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		})
	}
}

func seedBusyQueue(t *testing.T, p Params) {
	t.Helper()
	store, err := redemption.OpenRedeemerDB(p.RedeemerDB)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Bind(t.Context(), redemption.Binding{ChainID: "1", Controller: *p.Controller, Role: "redeemer", Destination: "chain"}, redemption.DefaultLimits, nil))
	payer, err := eth.OpenKeystoreFile(test.WriteFixedKeystore(t))
	require.NoError(t, err)
	for nonce := uint32(1); nonce <= 3; nonce++ {
		random := new(big.Int).SetUint64(uint64(nonce))
		ticket := &pm.SignedTicket{Ticket: &pm.Ticket{PayerAddress: payer.Address(), Recipient: common.HexToAddress("0x5678"), FaceValue: big.NewInt(1e12), WinProb: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), TicketNonce: nonce, RecipientRandHash: crypto.Keccak256Hash(common.LeftPadBytes(random.Bytes(), 32)), CreationRound: 1, CreationRoundBlockHash: common.HexToHash("0x1"), ParamsExpirationBlock: big.NewInt(10)}, RecipientRand: random}
		ticket.Sig, err = payer.SignMessage(ticket.Hash().Bytes())
		require.NoError(t, err)
		require.NoError(t, store.Admit(t.Context(), ticket))
	}
}

func TestPrintConfigRedactsCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LIVEPEER_REDEEMER_RPC_URL", "http://private.example/secret-token")
	t.Setenv("LIVEPEER_REDEEMER_KEYSTORE_PASSWORD", "wallet-secret")
	t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", "https://example.com/?auth-url-secret")
	t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK_HEADERS", "Authorization: Bearer auth-header-secret")
	var out, errOut bytes.Buffer
	cmd := Root(&out, &errOut)
	cmd.SetArgs([]string{"--config=", "--print-config"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), "8941")
	require.NotContains(t, out.String(), "secret-token")
	require.NotContains(t, out.String(), "wallet-secret")
	require.NotContains(t, out.String(), "auth-url-secret")
	require.NotContains(t, out.String(), "auth-header-secret")
}

func loadAuthConfig(t *testing.T, p Params, args ...string) (Params, error) {
	t.Helper()
	cmd := nodeconfig.Command("redeemer", nodeconfig.Mainnet, boa.Cmd[Params]{Params: &p, RejectUnknown: true,
		RunFuncE: func(p *Params, _ *cobra.Command, _ []string) error { return p.Validate() },
	}, &p.ConfigFile)
	err := cmd.RunArgsE(args)
	return p, err
}

func TestAuthWebhookConfiguration(t *testing.T) {
	base := serviceParams(t, test.NewRedemptionRPC(t))
	const endpoint = "https://webhook.example/auth?token=private-detail"
	const headers = "Authorization: Bearer private-detail"
	for _, tc := range []struct {
		name, field, content string
		withURL              bool
	}{
		{"empty URL file", "AUTH_WEBHOOK_FILE", "", false},
		{"empty orphan headers file", "AUTH_WEBHOOK_HEADERS_FILE", "", false},
		{"blank orphan headers file", "AUTH_WEBHOOK_HEADERS_FILE", " \t\n", false},
		{"empty headers with URL", "AUTH_WEBHOOK_HEADERS_FILE", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(file, []byte(tc.content), 0600))
			t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", "")
			t.Setenv("LIVEPEER_REDEEMER_"+tc.field, file)
			if tc.withURL {
				t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", endpoint)
			}
			_, err := loadAuthConfig(t, base, "--config=")
			if tc.withURL {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "auth webhook must be an absolute HTTP or HTTPS URL")
				require.NotContains(t, err.Error(), "private-detail")
			}
		})
	}
	for _, source := range []string{"environment", "files"} {
		t.Run(source, func(t *testing.T) {
			args := []string{"--config="}
			if source == "environment" {
				t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", endpoint)
				t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK_HEADERS", headers)
			} else {
				dir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "url"), []byte(endpoint), 0600))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "headers"), []byte(headers), 0600))
				path := filepath.Join(dir, "config.toml")
				test.WriteConfig(t, path, "AuthWebhookFile = 'url'\nAuthWebhookHeadersFile = 'headers'\n")
				args = []string{"--config", path}
			}
			p, err := loadAuthConfig(t, base, args...)
			require.NoError(t, err)
			require.Equal(t, endpoint, p.AuthWebhook.String())
			require.Equal(t, "Bearer private-detail", http.Header(p.AuthWebhookHeaders).Get("Authorization"))
		})
	}
	for _, flag := range []string{"--auth-webhook", "--auth-webhook-headers"} {
		_, err := loadAuthConfig(t, base, "--config=", flag, "private-detail")
		require.ErrorContains(t, err, "unknown flag")
		require.NotContains(t, err.Error(), "private-detail")
	}
	for field, value := range map[string]string{"AUTH_WEBHOOK": endpoint, "AUTH_WEBHOOK_HEADERS": headers} {
		t.Run(field+" file protection", func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(file, []byte(value), 0600))
			t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", endpoint)
			t.Setenv("LIVEPEER_REDEEMER_"+field+"_FILE", file)
			t.Setenv("LIVEPEER_REDEEMER_"+field, value)
			_, err := loadAuthConfig(t, base, "--config=")
			require.ErrorContains(t, err, "cannot both be set")
			require.NotContains(t, err.Error(), "private-detail")
			t.Setenv("LIVEPEER_REDEEMER_"+field, "")
			require.NoError(t, os.Chmod(file, 0644))
			_, err = loadAuthConfig(t, base, "--config=")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-detail")
		})
	}
}

func TestAuthWebhookConfigurationValidation(t *testing.T) {
	p := serviceParams(t, test.NewRedemptionRPC(t))
	require.NoError(t, p.Validate(), "omitted webhook is disabled")
	for _, endpoint := range []string{"/private-detail", "ftp://private-detail", "https://user:private-detail@example.com", "https://example.com/#private-detail"} {
		p.AuthWebhook, _ = url.Parse(endpoint)
		err := p.Validate()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private-detail")
	}
	p.AuthWebhook = nil
	p.AuthWebhookHeaders = nodeconfig.Headers{"Authorization": {"private-detail"}}
	require.Error(t, p.Validate(), "headers require an endpoint")
	p.AuthWebhook, _ = url.Parse("http://127.0.0.1/private?token=private-detail")
	require.NoError(t, p.Validate())
	p.AuthWebhookHeaders = nodeconfig.Headers{"Bad Name": {"private-detail"}}
	err := p.Validate()
	require.ErrorContains(t, err, "invalid auth webhook headers")
	require.NotContains(t, err.Error(), "private-detail")

}

type authTransport func(*http.Request) (*http.Response, error)

func (f authTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	ticket := redemption.TicketJSON{
		Payer: common.HexToAddress("0x1234"), Recipient: common.HexToAddress("0x5678"),
		FaceValue: "1000", WinProb: "42", Nonce: 7,
		RandHash: common.HexToHash("0x1234"), CreationRound: "1", RoundHash: common.HexToHash("0x5678"),
		Expiration: "10", Signature: []byte{1, 2, 3}, Randomness: "9",
	}
	for _, submitted := range []*redemption.TicketJSON{nil, &ticket} {
		raw, err := json.Marshal(submitted)
		require.NoError(t, err)
		var expected map[string]any
		require.NoError(t, json.Unmarshal(raw, &expected))
		delete(expected, "recipient_rand")
		want, err := json.Marshal(expected)
		require.NoError(t, err)
		r := httptest.NewRequest("POST", "/tickets", nil)
		r.Header = http.Header{"Authorization": {"Bearer caller-secret"}, "X-Caller": {"one", "two"}}
		require.True(t, auth.authorize(httptest.NewRecorder(), r, submitted))
		payload := <-received
		require.Equal(t, r.Header, payload.Headers)
		require.JSONEq(t, string(want), string(payload.Ticket), "only recipient_rand is omitted")
	}
	require.Equal(t, "9", ticket.Randomness, "redemption keeps the original randomness")
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
		{"unavailable", "", 0, 502, "error"},
		{"timeout", "", 0, 502, "error"},
		{"cancelled", "", 0, 502, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logs bytes.Buffer
				calls := 0
				auth := testAuthWebhook(t, "http://webhook.example?private-detail", nodeconfig.Headers{"Authorization": {"private-detail"}}, &logs)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				auth.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					switch tc.name {
					case "unavailable":
						return nil, errors.New("private-detail")
					case "cancelled":
						cancel()
						fallthrough
					case "timeout":
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					var body io.Reader = strings.NewReader(tc.body)
					if tc.name == "read failure" {
						body = io.MultiReader(body, iotest.ErrReader(errors.New("private-detail")))
					}
					return &http.Response{StatusCode: tc.upstream, Body: io.NopCloser(body)}, nil
				})
				w := httptest.NewRecorder()
				started := time.Now()
				require.Equal(t, tc.want == 200, auth.authorize(w, httptest.NewRequest("POST", "/tickets", nil).WithContext(ctx), nil))
				if tc.name == "timeout" {
					require.Equal(t, 5*time.Second, time.Since(started))
				}
				require.Equal(t, tc.want, w.Code)
				require.Equal(t, 1, calls, "authorization never retries")
				require.NotContains(t, logs.String()+w.Body.String(), "private-detail")
				require.EqualValues(t, 1, test.MetricSum(t, auth.metrics.Registry, "redeemer_auth_webhook_requests_total", map[string]string{"status": strconv.Itoa(tc.upstream), "outcome": tc.outcome}))
				require.EqualValues(t, 1, test.MetricSum(t, auth.metrics.Registry, "redeemer_auth_webhook_duration_seconds", nil))
			})
		})
	}
}

func TestAuthWebhookRedirectAndReplay(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch calls.Add(1) {
		case 1:
			http.Redirect(w, r, "/redirected", http.StatusFound)
			return
		case 3:
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close() // Lose the response on an already pooled connection.
			return
		}
		_, _ = io.WriteString(w, `{"status":200}`)
	}))
	defer server.Close()
	auth := testAuthWebhook(t, server.URL, nodeconfig.Headers{"Idempotency-Key": {"configured"}}, io.Discard)
	for i, allowed := range []bool{false, true, false} {
		w := httptest.NewRecorder()
		require.Equal(t, allowed, auth.authorize(w, httptest.NewRequest("POST", "/tickets", nil), nil))
		if !allowed {
			require.Equal(t, 502, w.Code)
		}
		require.EqualValues(t, i+1, calls.Load(), "no redirects or replays")
	}
}

func TestAuthorizationAndQueuedRedemption(t *testing.T) {
	f := test.NewRedemptionRPC(t)
	p := serviceParams(t, f)
	synctest.Test(t, func(t *testing.T) {
		rpc, err := eth.NewRPC(p.RPCURL, authTransport(func(r *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			f.Serve(w, r)
			return w.Result(), nil
		}))
		require.NoError(t, err)
		defer rpc.Close()
		contracts, err := eth.NewContracts(rpc, *p.Controller)
		require.NoError(t, err)
		chain := eth.PaymentChain{Contracts: contracts}
		store, err := redemption.OpenRedeemerDB(p.RedeemerDB)
		require.NoError(t, err)
		defer store.Close()
		require.NoError(t, store.Bind(t.Context(), redemption.Binding{ChainID: "1", Controller: *p.Controller, Role: "redeemer", Destination: "chain"}, redemption.Limits{Pending: 1, PerRecipient: 1}, chain))
		key, err := eth.OpenKeystoreFile(p.KeystoreFile, p.KeystorePasswordFile)
		require.NoError(t, err)
		input := redemption.TicketJSON{
			Payer: key.Address(), Recipient: common.HexToAddress("0x5678"), FaceValue: "1000000000000",
			WinProb: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String(), Nonce: 1,
			RandHash: crypto.Keccak256Hash(common.LeftPadBytes([]byte{1}, 32)), CreationRound: "1",
			RoundHash: common.HexToHash("0x1"), Expiration: "10", Randomness: "1",
		}
		ticket, err := input.SignedTicket()
		require.NoError(t, err)
		input.Signature, err = key.SignMessage(ticket.Hash().Bytes())
		require.NoError(t, err)
		f.Tickets[ticket.Hash()], f.Finalized = ticket.RecipientRandHash, false
		decision, calls := 409, 0
		unavailable := false
		auth := testAuthWebhook(t, "http://webhook.example", nil, io.Discard)
		auth.client.Transport = authTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if unavailable {
				return nil, errors.New("unavailable")
			}
			var payload struct{ Ticket redemption.TicketJSON }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
			require.Empty(t, payload.Ticket.Randomness)
			payload.Ticket.Randomness = "0" // Verify the signature without the preimage.
			signed, err := payload.Ticket.SignedTicket()
			require.NoError(t, err)
			require.True(t, (pm.DefaultSigVerifier{}).Verify(signed.PayerAddress, signed.Hash().Bytes(), signed.Sig))
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":` + strconv.Itoa(decision) + `}`))}, nil
		})
		body, err := json.Marshal(redemption.TicketRequest{ChainID: "1", Controller: *p.Controller, Ticket: input})
		require.NoError(t, err)
		handler := redemption.HandlerWithAuthorization(store, auth.authorize)
		submit := func() int {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("POST", "/tickets", bytes.NewReader(body)))
			return w.Code
		}
		require.Equal(t, 403, submit())
		count, err := store.WinningTicketCount(input.Payer)
		require.NoError(t, err)
		require.Zero(t, count, "denied requests do not store tickets")
		decision = 200
		require.Equal(t, 202, submit())
		require.Equal(t, 202, submit(), "duplicates succeed at capacity")
		decision = 403
		require.Equal(t, 403, submit(), "duplicates are authorized again")
		unavailable = true
		require.Equal(t, 502, submit())
		chainDest, err := redemption.NewChainDestination(t.Context(), store, chain, key, big.NewInt(1), redemption.RedemptionOptions{Timeout: 5 * time.Second, MaxReplacements: 1})
		require.NoError(t, err)
		worker, err := redemption.NewWorker(store, chain, chainDest)
		require.NoError(t, err)
		require.Empty(t, worker.Process(t.Context()))
		synctest.Wait()
		require.Len(t, f.Transactions(), 1)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		sent := f.Transactions()
		require.Len(t, sent, 2)
		require.Equal(t, sent[0].Nonce(), sent[1].Nonce())
		require.Equal(t, sent[0].Data(), sent[1].Data())
		require.Equal(t, 5, calls, "background redemption never authorizes again")
	})
}
