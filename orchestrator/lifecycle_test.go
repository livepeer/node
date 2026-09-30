package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livepeer/node/destination"
	"github.com/stretchr/testify/require"
)

func TestReleaseCancelsActiveProxy(t *testing.T) {
	for _, mode := range []string{"persistent", "single-shot"} {
		for _, protocol := range []string{"http", "sse", "websocket"} {
			for _, issuer := range []string{"client", "runner", "shutdown"} {
				t.Run(mode+"/"+protocol+"/"+issuer, func(t *testing.T) {
					started := make(chan http.Header, 1)
					ended := make(chan struct{})
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer close(ended)
						started <- r.Header.Clone()
						switch protocol {
						case "websocket":
							upgrader := websocket.Upgrader{}
							conn, err := upgrader.Upgrade(w, r, nil)
							if err != nil {
								return
							}
							defer conn.Close()
							_, _, _ = conn.ReadMessage()
						case "sse":
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, "data: started\n\n")
							w.(http.Flusher).Flush()
							<-r.Context().Done()
						default:
							<-r.Context().Done()
						}
					}))
					defer upstream.Close()
					policy, err := destination.New("runner", []string{strings.TrimPrefix(upstream.URL, "http://")})
					require.NoError(t, err)
					registry := NewRegistry("bootstrap", "http://orchestrator.example", time.Second, time.Minute)
					require.NoError(t, registry.AddStatic(StaticRunner{ID: "runner", RunnerURL: upstream.URL, App: "test", Mode: mode, Capacity: 1}))
					app := NewServer(registry, policy, policy, slog.New(slog.NewTextHandler(io.Discard, nil)))
					server := httptest.NewServer(app)
					defer server.Close()
					defer app.Close()
					path := "/apps/runner/app"
					if mode == "persistent" {
						sid, _, _, _, err := registry.Reserve("runner")
						require.NoError(t, err)
						path = "/apps/runner/session/" + sid + "/app"
					}
					clientDone := make(chan struct{})
					if protocol == "websocket" {
						conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+path, nil)
						require.NoError(t, err)
						defer conn.Close()
						go func() { defer close(clientDone); _, _, _ = conn.ReadMessage() }()
					} else {
						go func() {
							defer close(clientDone)
							response, err := http.Get(server.URL + path)
							if err == nil {
								defer response.Body.Close()
								_, _ = io.Copy(io.Discard, response.Body)
							}
						}()
					}
					var headers http.Header
					select {
					case headers = <-started:
					case <-time.After(3 * time.Second):
						t.Fatal("upstream did not start")
					}
					sid := headers.Get("Livepeer-Session-Id")
					require.NotEmpty(t, sid)
					require.NotEmpty(t, headers.Get("Livepeer-Session-Token"))
					if issuer == "shutdown" {
						app.Close()
					} else {
						stopPath := "/apps/runner/session/" + sid + "/stop"
						if issuer == "runner" {
							stopPath = "/runner/runner/session/" + sid + "/stop"
						}
						request, err := http.NewRequest(http.MethodPost, server.URL+stopPath, nil)
						require.NoError(t, err)
						request.Header.Set("Livepeer-Session-Token", headers.Get("Livepeer-Session-Token"))
						response, err := http.DefaultClient.Do(request)
						require.NoError(t, err)
						require.Equal(t, http.StatusOK, response.StatusCode)
						require.NoError(t, response.Body.Close())
					}

					select {
					case <-ended:
					case <-time.After(3 * time.Second):
						t.Fatal("release did not cancel upstream")
					}
					select {
					case <-clientDone:
					case <-time.After(3 * time.Second):
						t.Fatal("release did not close client stream")
					}
					_, sessions := registry.Counts()
					require.Zero(t, sessions)
				})
			}
		}
	}
}

func TestRegistryTeardownCancelsSessions(t *testing.T) {
	for _, reason := range []string{"release", "expired", "unregistered", "shutdown"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				registry := NewRegistry("bootstrap", "http://orch.example", time.Second, 3*time.Second)
				registered, _, err := registry.Heartbeat(heartbeatRequest{RunnerURL: "https://runner.example", App: "test", Capacity: 1}, "bootstrap")
				require.NoError(t, err)
				sid, _, _, _, err := registry.Reserve(registered.RunnerID)
				require.NoError(t, err)
				ctx, ok := registry.sessionContext(registered.RunnerID, sid)
				require.True(t, ok)
				switch reason {
				case "release":
					registry.ReleaseBySession(sid)
				case "expired":
					synctest.Sleep(4 * time.Second)
					registry.Expire()
				case "unregistered":
					_, err = registry.Unregister(registered.RunnerID, registered.HeartbeatSecret)
					require.NoError(t, err)
				case "shutdown":
					registry.Close()
				}
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			})
		})
	}
}
