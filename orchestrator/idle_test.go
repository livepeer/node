package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/internal/test"
	"github.com/stretchr/testify/require"
)

func TestSessionIdleAdmission(t *testing.T) {
	for _, scenario := range []struct {
		name, priceUnit, mode string
		disabled              bool
	}{
		{name: "free"},
		{name: "fixed", priceUnit: "fixed"},
		{name: "time-priced", priceUnit: "hour"},
		{name: "disabled", disabled: true},
		{name: "single-shot", mode: "single-shot"},
		{name: "paid-single-shot", priceUnit: "fixed", mode: "single-shot"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const upstream = "http://127.0.0.1:1"
				reg, s := reviewServer(t, upstream)
				defer s.Close()
				if !scenario.disabled {
					reg.idleTimeout = 30 * time.Second
				}
				runner := StaticRunner{ID: "r", RunnerURL: upstream, App: "test", Mode: scenario.mode}
				var agreed *appQuote
				if scenario.priceUnit != "" {
					reg.SetWeiPerUSD(big.NewRat(3600, 1))
					runner.PriceInfo = priceInfo{Price: "1", Currency: "usd", Unit: scenario.priceUnit}
				}
				require.NoError(t, reg.AddStatic(runner))
				if scenario.priceUnit != "" {
					quote, _, err := reg.quoteForApp("r")
					require.NoError(t, err)
					agreed = &quote
				}

				sid, _, _, _, err := reg.reserveWithPrice("r", "session", agreed, scenario.mode)
				require.NoError(t, err)
				ctx, ok := reg.sessionContext("r", sid)
				require.True(t, ok)
				if agreed != nil {
					time.Sleep(time.Minute)
					require.NoError(t, ctx.Err())
					require.True(t, reg.activateSession("r", sid))
				}
				time.Sleep(30*time.Second - time.Nanosecond)
				require.NoError(t, ctx.Err())
				if scenario.disabled || scenario.mode == "single-shot" {
					_, token, _, err := reg.sessionTarget("r", sid)
					require.NoError(t, err)
					sess, counted := reg.beginProxyRequest("r", sid, token)
					require.NotNil(t, sess)
					require.False(t, counted)
					time.Sleep(time.Minute)
					require.NoError(t, ctx.Err())
					return
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.EqualValues(t, 1, test.MetricSum(t, reg.metrics.Registry, "session_ends_total", map[string]string{"reason": "idle_timeout"}))
			})
		})
	}
}

func TestSessionIdleProxyRoutes(t *testing.T) {
	for _, route := range []struct {
		name, template string
	}{
		{name: "primary"},
		{name: "generated"},
		{name: "legacy-alias"},
		{name: "path-template", template: "https://example.com/public/{proxy}"},
		{name: "host-template", template: "https://{proxy}.apps.example.com"},
		{name: "callback-proxy"},
	} {
		t.Run(route.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const upstream = "http://127.0.0.1:1"
				reg, s := reviewServer(t, upstream)
				defer s.Close()
				reg.idleTimeout = 30 * time.Second
				reg.proxyTemplate = route.template
				require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: upstream, App: "test", Proxy: route.name != "primary"}))
				sid, appURL, _, _, err := reg.Reserve("r")
				require.NoError(t, err)
				_, token, _, err := reg.sessionTarget("r", sid)
				require.NoError(t, err)
				if route.name == "legacy-alias" {
					appURL = strings.Replace(appURL, "/run/", "/proxy/", 1)
				}
				if route.name == "callback-proxy" {
					req := httptest.NewRequest(http.MethodPost, "/runner/r/session/"+sid+"/proxy", strings.NewReader(`{"target_url":"http://127.0.0.1:1"}`))
					req.Header.Set("Livepeer-Session-Token", token)
					w := httptest.NewRecorder()
					s.ServeHTTP(w, req)
					require.Equal(t, http.StatusOK, w.Code, w.Body.String())
					var proxy map[string]string
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &proxy))
					appURL = proxy["url"]
				}

				calls := 0
				reply := roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: http.NoBody}, nil
				})
				s.runnerTransport.RegisterProtocol("http", reply)
				s.proxyTransport.RegisterProtocol("http", reply)
				request := func() int {
					w := httptest.NewRecorder()
					s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, appURL+"/work", nil))
					return w.Code
				}
				time.Sleep(30*time.Second - time.Nanosecond)
				require.Equal(t, http.StatusInternalServerError, request())
				time.Sleep(time.Nanosecond)
				ctx, ok := reg.sessionContext("r", sid)
				require.True(t, ok, "the forwarded error must restart the idle period")

				// Leave the deadline in force even if the timer callback has not run.
				reg.mu.Lock()
				stopped := reg.apps["r"].Sessions[sid].idleTimer.Stop()
				reg.mu.Unlock()
				require.True(t, stopped)
				time.Sleep(30*time.Second - time.Nanosecond)
				if route.name == "host-template" {
					time.Sleep(time.Nanosecond)
				}
				require.Equal(t, http.StatusNotFound, request())
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.Equal(t, 1, calls, "the request at or after the deadline must not be forwarded")
			})
		})
	}
}

func TestSessionIdleRejectedProxyRequest(t *testing.T) {
	for _, scenario := range []struct {
		name, method, upstream string
		lookupDelay            time.Duration
	}{
		{"unsupported CONNECT", http.MethodConnect, "http://127.0.0.1:1", 0},
		{"destination denied", http.MethodGet, "http://127.0.0.1:1", 0},
		{"DNS failure after expiry", http.MethodGet, "http://runner.example", 2 * time.Nanosecond},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reg, s := reviewServer(t, "http://127.0.0.1:1")
				defer s.Close()
				reg.idleTimeout = 30 * time.Second
				deny, err := destination.New("runner", nil)
				require.NoError(t, err)
				deny.Lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
					time.Sleep(scenario.lookupDelay)
					return nil, &net.DNSError{Err: "lookup failed", Name: host}
				}
				s.runnerTransport = deny.Transport(time.Second)
				require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: scenario.upstream, App: "test"}))
				sid, _, _, _, err := reg.Reserve("r")
				require.NoError(t, err)
				ctx, ok := reg.sessionContext("r", sid)
				require.True(t, ok)
				time.Sleep(30*time.Second - time.Nanosecond)
				w := httptest.NewRecorder()
				s.ServeHTTP(w, httptest.NewRequest(scenario.method, "/apps/r/session/"+sid+"/app", nil))
				require.Equal(t, http.StatusBadGateway, w.Code)
				if scenario.lookupDelay == 0 {
					time.Sleep(time.Nanosecond)
					synctest.Wait()
				}
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			})
		})
	}
}

func TestSessionIdleOverlappingLongPollAndSSE(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const upstream = "http://127.0.0.1:1"
		reg, s := reviewServer(t, upstream)
		defer s.Close()
		reg.idleTimeout = 30 * time.Second
		require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: upstream, App: "test", Proxy: true}))
		sid, appURL, _, _, err := reg.Reserve("r")
		require.NoError(t, err)
		sessionCtx, ok := reg.sessionContext("r", sid)
		require.True(t, ok)
		longPoll := make(chan struct{})
		body, stream := io.Pipe()
		defer stream.Close()
		s.runnerTransport.RegisterProtocol("http", roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/poll" {
				select {
				case <-longPoll:
					return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}, nil
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, ContentLength: -1}, nil
		}))
		pollDone, sseDone := make(chan struct{}), make(chan struct{})
		poll, events := httptest.NewRecorder(), httptest.NewRecorder()
		go func() {
			defer close(pollDone)
			s.ServeHTTP(poll, httptest.NewRequest(http.MethodGet, "/apps/r/session/"+sid+"/app/poll", nil))
		}()
		go func() {
			defer close(sseDone)
			s.ServeHTTP(events, httptest.NewRequest(http.MethodGet, appURL+"/events", nil))
		}()
		synctest.Wait()
		time.Sleep(time.Minute)
		require.NoError(t, sessionCtx.Err())
		select {
		case <-pollDone:
			t.Fatalf("long poll finished before its upstream response with status %d", poll.Code)
		default:
		}
		close(longPoll)
		<-pollDone
		require.Equal(t, http.StatusNoContent, poll.Code)
		time.Sleep(time.Minute)
		require.NoError(t, sessionCtx.Err())
		_, err = io.WriteString(stream, "data: still open\n\n")
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		<-sseDone
		require.Equal(t, http.StatusOK, events.Code)
		require.Equal(t, "data: still open\n\n", events.Body.String())
		time.Sleep(30*time.Second - time.Nanosecond)
		require.NoError(t, sessionCtx.Err())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		require.ErrorIs(t, sessionCtx.Err(), context.Canceled)
	})
}

type idleUpgradeWriter struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (w idleUpgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

func TestSessionIdleWebSocketDisconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const upstream = "http://127.0.0.1:1"
		reg, s := reviewServer(t, upstream)
		defer s.Close()
		reg.idleTimeout = 30 * time.Second
		require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: upstream, App: "test"}))
		sid, appURL, _, _, err := reg.Reserve("r")
		require.NoError(t, err)
		sessionCtx, ok := reg.sessionContext("r", sid)
		require.True(t, ok)
		client, downstream := net.Pipe()
		defer client.Close()
		backend, provider := net.Pipe()
		defer provider.Close()
		s.runnerTransport.RegisterProtocol("http", roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusSwitchingProtocols,
				Header:     http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}},
				Body:       backend,
			}, nil
		}))
		req := httptest.NewRequest(http.MethodGet, appURL, nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.ServeHTTP(idleUpgradeWriter{httptest.NewRecorder(), downstream}, req)
		}()
		require.NoError(t, client.SetReadDeadline(time.Now().Add(time.Second)))
		response, err := http.ReadResponse(bufio.NewReader(client), req)
		require.NoError(t, err)
		require.Equal(t, http.StatusSwitchingProtocols, response.StatusCode)
		time.Sleep(time.Minute)
		require.NoError(t, sessionCtx.Err())
		require.NoError(t, client.Close())
		<-done
		time.Sleep(30*time.Second - time.Nanosecond)
		require.NoError(t, sessionCtx.Err())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		require.ErrorIs(t, sessionCtx.Err(), context.Canceled)
	})
}

func TestSessionIdleReleasesResources(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const upstream = "http://127.0.0.1:1"
		reg, s := reviewServer(t, upstream)
		defer s.Close()
		reg.idleTimeout = 30 * time.Second
		registration := heartbeatRequest{RunnerID: "r", RunnerURL: upstream, App: "test", Proxy: true}
		w := heartbeatHTTP(t, s, registration, "bootstrap")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var owner heartbeatResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &owner))
		require.Equal(t, http.StatusOK, heartbeatHTTP(t, s, registration, owner.HeartbeatSecret).Code)
		sid, appURL, _, _, err := reg.Reserve("r")
		require.NoError(t, err)
		_, token, _, err := reg.sessionTarget("r", sid)
		require.NoError(t, err)
		callback := "/runner/r/session/" + sid
		create := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, callback+"/channels", strings.NewReader(`{"channels":[{"name":"events"}]}`))
			req.Header.Set("Livepeer-Session-Token", token)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, req)
			return w
		}
		w = create()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var channel struct{ Channels []trickleChannel }
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &channel))
		require.Len(t, channel.Channels, 1)
		get := func(url string) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
			return w
		}
		_, _, _, status, err := reg.Reserve("r")
		require.ErrorIs(t, err, errRunnerCapacity)
		require.Equal(t, http.StatusServiceUnavailable, status)

		time.Sleep(30*time.Second - time.Nanosecond)
		require.Equal(t, http.StatusOK, heartbeatHTTP(t, s, registration, owner.HeartbeatSecret).Code)
		require.Equal(t, http.StatusOK, get(channel.Channels[0].URL+"/next").Code)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		require.Equal(t, http.StatusNotFound, get(appURL).Code)
		require.Equal(t, http.StatusNotFound, get(channel.Channels[0].URL+"/next").Code)
		require.Equal(t, http.StatusUnauthorized, create().Code)
		w = get(owner.O2R.URL + "/1")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var event map[string]string
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &event))
		require.Equal(t, "released", event["event"])
		require.Equal(t, sid, event["session"])
		require.Equal(t, "2", get(owner.O2R.URL+"/next").Body.String(), "one reservation and one release")
		_, _, _, _, err = reg.Reserve("r")
		require.NoError(t, err, "capacity should be available again")
	})
}

func TestSessionIdleCallbacksAndSessionIDReuse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const upstream = "http://127.0.0.1:1"
		reg, s := reviewServer(t, upstream)
		defer s.Close()
		reg.idleTimeout = 30 * time.Second
		require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: upstream, App: "test"}))
		const sid = "reused"
		reserve := func() string {
			_, _, _, _, err := reg.ReserveWithID("r", sid)
			require.NoError(t, err)
			_, token, _, err := reg.sessionTarget("r", sid)
			require.NoError(t, err)
			return token
		}
		token := reserve()
		old, counted := reg.beginProxyRequest("r", sid, token)
		require.NotNil(t, old)
		require.True(t, counted)
		reg.ReleaseBySession(sid, "client_stop")
		newToken := reserve()
		stale, counted := reg.beginProxyRequest("r", sid, token)
		require.Nil(t, stale)
		require.False(t, counted)
		reg.endProxyRequest("r", old, true)
		current, counted := reg.beginProxyRequest("r", sid, newToken)
		require.NotNil(t, current)
		require.True(t, counted)
		reg.endProxyRequest("r", current, true)
		released := make(chan struct{})
		time.AfterFunc(30*time.Second, func() {
			reg.ReleaseBySession(sid, "client_stop")
			close(released)
		})
		<-released
		synctest.Wait()
		require.ErrorIs(t, current.ctx.Err(), context.Canceled)
		require.EqualValues(t, 2, test.MetricSum(t, reg.metrics.Registry, "session_ends_total", nil))
		reserve()
	})
}
