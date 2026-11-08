package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/signer"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

// The TestConformance tests follow the client session flow in docs/http-api.md
// as an outside integrator would: plain HTTP on real listeners, no SDK, and JSON
// checked against docs/*.schema.json. Only the chain is fake, and the clock is
// moved only to exhaust a balance. Change these tests with the docs and schemas.

const conformanceApp = "conformance"

type conformanceResponse struct {
	status int
	body   []byte
}

func conformanceRequest(ctx context.Context, t *testing.T, method, target string, header map[string]string, body []byte) conformanceResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	require.NoError(t, err)
	for key, value := range header {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return conformanceResponse{resp.StatusCode, data}
}

func conformanceSchemaError(t *testing.T, schema string, data []byte) error {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	compiled, err := compiler.Compile("../docs/" + schema)
	require.NoError(t, err)
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&value), string(data))
	return compiled.Validate(value)
}

func requireConformanceSchema(t *testing.T, schema string, data []byte) {
	t.Helper()
	require.NoError(t, conformanceSchemaError(t, schema, data), string(data))
}

type conformanceOrchestrator struct {
	url    string
	o2r    string
	server *Server
}

// startConformanceOrchestrator listens first so that advertised URLs, including
// the challenge's payment_url, are the ones the client really calls.
func startConformanceOrchestrator(ctx context.Context, t *testing.T, runnerURL, price string, capacity int, configure func(*Registry, *Server)) *conformanceOrchestrator {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	service := "http://" + listener.Addr().String()
	target, err := url.Parse(runnerURL)
	require.NoError(t, err)
	policy, err := destination.New("runner", []string{target.Host})
	require.NoError(t, err)
	registry := NewRegistry("bootstrap", service, time.Minute, time.Hour)
	server := NewServer(registry, policy, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(server.Close)
	if configure != nil {
		configure(registry, server)
	}
	listening := httptest.NewUnstartedServer(server)
	require.NoError(t, listening.Listener.Close())
	listening.Listener = listener
	listening.Start()
	t.Cleanup(listening.Close)

	body := fmt.Sprintf(`{"apps":[{"id":%q,"app":%q,"runner_url":%q,"mode":"persistent","status":"ready","capacity":%d,"price_info":%s}]}`, conformanceApp, conformanceApp, runnerURL, capacity, price)
	registered := conformanceRequest(ctx, t, http.MethodPost, service+"/runners/heartbeat", map[string]string{"Authorization": "bootstrap"}, []byte(body))
	require.Equal(t, http.StatusOK, registered.status, string(registered.body))
	var heartbeat struct {
		O2R struct {
			URL string `json:"url"`
		} `json:"o2r"`
	}
	require.NoError(t, json.Unmarshal(registered.body, &heartbeat))
	require.NotEmpty(t, heartbeat.O2R.URL)
	return &conformanceOrchestrator{url: service, o2r: heartbeat.O2R.URL, server: server}
}

// reservationURL is step 1 of the flow: a persistent runner's discovery URL.
func (o *conformanceOrchestrator) reservationURL(ctx context.Context, t *testing.T) string {
	t.Helper()
	discovered := conformanceRequest(ctx, t, http.MethodGet, o.url+"/discovery", nil, nil)
	require.Equal(t, http.StatusOK, discovered.status, string(discovered.body))
	requireConformanceSchema(t, "discovery.schema.json", discovered.body)
	var entries []struct {
		Runners []struct {
			URL string `json:"url"`
			App string `json:"app"`
		} `json:"runners"`
	}
	require.NoError(t, json.Unmarshal(discovered.body, &entries))
	require.Len(t, entries, 1)
	require.Len(t, entries[0].Runners, 1)
	require.Equal(t, conformanceApp, entries[0].Runners[0].App)
	return entries[0].Runners[0].URL
}

type conformanceEvent struct {
	Event        string `json:"event"`
	AppID        string `json:"app_id"`
	Session      string `json:"session"`
	SessionToken string `json:"session_token"`
}

func (o *conformanceOrchestrator) event(ctx context.Context, t *testing.T, sequence int) conformanceEvent {
	t.Helper()
	response := conformanceRequest(ctx, t, http.MethodGet, fmt.Sprintf("%s/%d", o.o2r, sequence), nil, nil)
	require.Equal(t, http.StatusOK, response.status, string(response.body))
	var event conformanceEvent
	require.NoError(t, json.Unmarshal(response.body, &event), string(response.body))
	return event
}

type conformanceSession struct {
	SessionID  string `json:"session_id"`
	AppURL     string `json:"app_url"`
	ControlURL string `json:"control_url"`
}

func decodeConformanceSession(t *testing.T, response conformanceResponse) conformanceSession {
	t.Helper()
	require.Equal(t, http.StatusOK, response.status, string(response.body))
	requireConformanceSchema(t, "session.schema.json", response.body)
	var session conformanceSession
	require.NoError(t, json.Unmarshal(response.body, &session))
	return session
}

// A live upload and its output are two ordinary requests that the runner links by
// session ID. Each chunk comes back while the upload is still open, so this holds
// only if the orchestrator streams both requests instead of buffering them.
func TestConformanceLiveMediaOverHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type forwardedRequest struct {
		path, query string
		header      http.Header
	}
	forwarded := make(chan forwardedRequest, 2)
	media := make(chan []byte, 8)
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- forwardedRequest{r.URL.Path, r.URL.RawQuery, r.Header.Clone()}
		switch r.Method + " " + r.URL.Path {
		case "PUT /base/in":
			defer close(media)
			buffer := make([]byte, 1024)
			for {
				n, err := r.Body.Read(buffer)
				if n > 0 {
					media <- bytes.Clone(buffer[:n])
				}
				if err != nil {
					return
				}
			}
		case "GET /base/out":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			for {
				select {
				case chunk, ok := <-media:
					if !ok {
						return
					}
					_, _ = w.Write(chunk)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer runner.Close()
	orchestrator := startConformanceOrchestrator(ctx, t, runner.URL+"/base", `{"price":0}`, 1, nil)

	reservation := orchestrator.reservationURL(ctx, t)
	session := decodeConformanceSession(t, conformanceRequest(ctx, t, http.MethodPost, reservation, nil, nil))

	full := conformanceRequest(ctx, t, http.MethodPost, reservation, nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, full.status, "capacity is one session")
	requireConformanceSchema(t, "orchestrator-error.schema.json", full.body)

	get, err := http.NewRequestWithContext(ctx, http.MethodGet, session.AppURL+"/out?rendition=720p", nil)
	require.NoError(t, err)
	output, err := http.DefaultClient.Do(get)
	require.NoError(t, err)
	defer output.Body.Close()
	require.Equal(t, http.StatusOK, output.StatusCode)

	upload, uploader := io.Pipe()
	defer uploader.Close()
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, session.AppURL+"/in", upload)
	require.NoError(t, err)
	uploadDone := make(chan struct{})
	go func() {
		defer close(uploadDone)
		if response, err := http.DefaultClient.Do(put); err == nil {
			_ = response.Body.Close()
		}
	}()
	for _, chunk := range [][]byte{[]byte("first chunk"), []byte("second chunk")} {
		_, err := uploader.Write(chunk)
		require.NoError(t, err)
		received := make([]byte, len(chunk))
		_, err = io.ReadFull(output.Body, received)
		require.NoError(t, err)
		require.Equal(t, chunk, received)
	}

	requests := []forwardedRequest{<-forwarded, <-forwarded}
	require.Equal(t, "/base/out", requests[0].path)
	require.Equal(t, "rendition=720p", requests[0].query)
	require.Equal(t, "/base/in", requests[1].path)
	token := requests[0].header.Get("Livepeer-Session-Token")
	require.NotEmpty(t, token)
	for _, request := range requests {
		require.Equal(t, conformanceApp, request.header.Get("Livepeer-Runner-Route"))
		require.Equal(t, session.SessionID, request.header.Get("Livepeer-Session-Id"))
		require.Equal(t, token, request.header.Get("Livepeer-Session-Token"))
		require.Equal(t, orchestrator.url+"/runner/"+conformanceApp+"/session/"+session.SessionID, request.header.Get("Livepeer-Session-Control"))
	}

	stopped := conformanceRequest(ctx, t, http.MethodPost, session.ControlURL+"/stop", nil, nil)
	require.Equal(t, http.StatusOK, stopped.status, string(stopped.body))
	_, _ = io.Copy(io.Discard, output.Body)
	require.NoError(t, ctx.Err(), "stop must end in-flight requests")
	require.NoError(t, uploader.Close())
	select {
	case <-uploadDone:
	case <-ctx.Done():
		t.Fatal("stop must end the upload")
	}

	gone := conformanceRequest(ctx, t, http.MethodGet, session.AppURL+"/out", nil, nil)
	require.Equal(t, http.StatusNotFound, gone.status)
	requireConformanceSchema(t, "orchestrator-error.schema.json", gone.body)

	reserved := orchestrator.event(ctx, t, 0)
	require.Equal(t, conformanceEvent{"reserved", conformanceApp, session.SessionID, token}, reserved)
	released := orchestrator.event(ctx, t, 1)
	require.Equal(t, conformanceEvent{"released", conformanceApp, session.SessionID, ""}, released)
}

// A paid live session, from the payer address through top-ups until the balance
// runs out. Requests to app_url carry no payment headers.
func TestConformancePaidLiveSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		if r.URL.Path == "/hold" {
			<-r.Context().Done()
		}
	}))
	defer runner.Close()

	recipient, payer := paymentTestKey(t), paymentTestKey(t)
	store, err := OpenRedeemerDB(filepath.Join(t.TempDir(), "recipient.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	// One ticket is worth just under 150 wei, and the runner costs 10 wei per second.
	winProb := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(2))
	engine, err := NewPaymentEngine(store, paymentTestChain{}, recipient.Address(), big.NewInt(150), winProb)
	require.NoError(t, err)
	signerService, err := signer.NewService(payer)
	require.NoError(t, err)
	t.Cleanup(func() { _ = signerService.Close() })
	signerService.SetPaymentChain(paymentTestChain{})
	signerServer := httptest.NewServer(signerService)
	t.Cleanup(signerServer.Close)
	orchestrator := startConformanceOrchestrator(ctx, t, runner.URL, `{"price":3600,"currency":"usd","unit":"hour"}`, 1, func(registry *Registry, server *Server) {
		registry.SetWeiPerUSD(big.NewRat(10, 1))
		server.SetPayment(engine)
	})
	reservation := orchestrator.reservationURL(ctx, t)

	identity := conformanceRequest(ctx, t, http.MethodPost, signerServer.URL+"/sign-orchestrator-info", nil, nil)
	require.Equal(t, http.StatusOK, identity.status, string(identity.body))
	requireConformanceSchema(t, "signer-payment.schema.json#/$defs/signerInfo", identity.body)
	var signerInfo struct {
		Address string `json:"address"`
	}
	require.NoError(t, json.Unmarshal(identity.body, &signerInfo))

	missingPayer := conformanceRequest(ctx, t, http.MethodPost, reservation, nil, nil)
	require.Equal(t, http.StatusPaymentRequired, missingPayer.status)
	requireConformanceSchema(t, "orchestrator-error.schema.json", missingPayer.body)

	challenged := conformanceRequest(ctx, t, http.MethodPost, reservation, map[string]string{"Livepeer-Payer-Address": signerInfo.Address}, nil)
	require.Equal(t, http.StatusPaymentRequired, challenged.status, string(challenged.body))
	requireConformanceSchema(t, "payment-challenge.schema.json", challenged.body)
	var challenge Challenge
	require.NoError(t, json.Unmarshal(challenged.body, &challenge))

	var state json.RawMessage
	generate := func() conformanceResponse {
		t.Helper()
		request := map[string]any{"orchestrator": challenge.PaymentParams, "type": "live"}
		if state != nil {
			request["state"] = state
		}
		body, err := json.Marshal(request)
		require.NoError(t, err)
		requireConformanceSchema(t, "signer-payment.schema.json#/$defs/request", body)
		return conformanceRequest(ctx, t, http.MethodPost, signerServer.URL+"/generate-live-payment", nil, body)
	}
	payment := func() map[string]string {
		t.Helper()
		generated := generate()
		require.Equal(t, http.StatusOK, generated.status, string(generated.body))
		requireConformanceSchema(t, "signer-payment.schema.json#/$defs/response", generated.body)
		var signed struct {
			Payment  string          `json:"payment"`
			SegCreds string          `json:"segCreds"`
			State    json.RawMessage `json:"state"`
		}
		require.NoError(t, json.Unmarshal(generated.body, &signed))
		state = signed.State
		return map[string]string{"Livepeer-Payment": signed.Payment, "Livepeer-Segment": signed.SegCreds}
	}

	session := decodeConformanceSession(t, conformanceRequest(ctx, t, http.MethodPost, reservation, payment(), nil))
	require.Equal(t, challenge.ManifestID, session.SessionID)
	require.Equal(t, session.ControlURL+"/payment", challenge.PaymentURL)
	reserved := orchestrator.event(ctx, t, 0)
	require.Equal(t, "reserved", reserved.Event, "reserved must follow payment")
	require.Equal(t, session.SessionID, reserved.Session)

	proxied := conformanceRequest(ctx, t, http.MethodGet, session.AppURL+"/ping", nil, nil)
	require.Equal(t, http.StatusOK, proxied.status, "app_url needs no payment headers")

	before, err := engine.Balance(session.SessionID)
	require.NoError(t, err)
	topUp := conformanceRequest(ctx, t, http.MethodPost, challenge.PaymentURL, payment(), nil)
	require.Equal(t, http.StatusOK, topUp.status, string(topUp.body))
	requireConformanceSchema(t, "payment-challenge.schema.json", topUp.body)
	after, err := engine.Balance(session.SessionID)
	require.NoError(t, err)
	require.Positive(t, after.Cmp(before), "a top-up must add credit")

	// The two payments carry more unused credit than one ticket is worth.
	skipped := generate()
	require.Equal(t, 482, skipped.status, string(skipped.body))
	requireConformanceSchema(t, "signer-payment.schema.json#/$defs/error", skipped.body)

	get, err := http.NewRequestWithContext(ctx, http.MethodGet, session.AppURL+"/hold", nil)
	require.NoError(t, err)
	held, err := http.DefaultClient.Do(get)
	require.NoError(t, err)
	defer held.Body.Close()
	require.Equal(t, http.StatusOK, held.StatusCode)

	// Half an hour without payment is far more than the balance covers.
	engine.mu.Lock()
	engine.sessions[session.SessionID].lastCharge = time.Now().Add(-30 * time.Minute)
	engine.mu.Unlock()
	orchestrator.server.ChargePaidSessions(ctx)

	_, _ = io.Copy(io.Discard, held.Body)
	require.NoError(t, ctx.Err(), "an empty balance must end in-flight requests")
	gone := conformanceRequest(ctx, t, http.MethodGet, session.AppURL+"/ping", nil, nil)
	require.Equal(t, http.StatusNotFound, gone.status)
	requireConformanceSchema(t, "orchestrator-error.schema.json", gone.body)
	released := orchestrator.event(ctx, t, 1)
	require.Equal(t, conformanceEvent{"released", conformanceApp, session.SessionID, ""}, released)
}

func TestConformanceSchemasRejectIncompleteMessages(t *testing.T) {
	for _, test := range []struct{ schema, body string }{
		{"session.schema.json", `{"session_id":"s","app_url":"https://orch.example/app"}`},
		{"session.schema.json", `{"session_id":"s","app_url":"ftp://orch.example/app","control_url":"https://orch.example"}`},
		{"payment-challenge.schema.json", `{"payment_params":"AA==","orchestrator":"https://orch.example","manifest_id":"m"}`},
		{"signer-payment.schema.json#/$defs/signerInfo", `{"address":"not-an-address","signature":"0x00"}`},
		{"signer-payment.schema.json#/$defs/request", `{"type":"live"}`},
		{"signer-payment.schema.json#/$defs/request", `{"orchestrator":"AA==","type":"other"}`},
		{"signer-payment.schema.json#/$defs/response", `{"payment":"AA==","state":{"state":"AA==","sig":"AA=="}}`},
		{"orchestrator-error.schema.json", `{"message":"missing error"}`},
		{"orchestrator-error.schema.json", `{"error":{"message":"signer format"}}`},
		{"signer-payment.schema.json#/$defs/error", `{"error":"orchestrator format"}`},
		{"signer-payment.schema.json#/$defs/error", `{"error":{"reason":"missing message"}}`},
	} {
		t.Run(strings.TrimSuffix(test.schema, ".schema.json")+"/"+test.body, func(t *testing.T) {
			require.Error(t, conformanceSchemaError(t, test.schema, []byte(test.body)))
		})
	}
}
