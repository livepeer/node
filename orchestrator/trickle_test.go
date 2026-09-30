package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDirectTrickleDeleteAllowsNamedChannelRecreation(t *testing.T) {
	reg, s := reviewServer(t, "http://127.0.0.1:1")
	require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test"}))
	sid, _, _, _, err := reg.Reserve("r")
	require.NoError(t, err)
	_, token, _, err := reg.sessionTarget("r", sid)
	require.NoError(t, err)
	create := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/runner/r/session/"+sid+"/channels", strings.NewReader(`{"channels":[{"name":"events"}]}`))
		req.Header.Set("Livepeer-Session-Token", token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)
		var body struct {
			Channels []struct {
				ChannelName string `json:"channel_name"`
			} `json:"channels"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.Len(t, body.Channels, 1)
		return body.Channels[0].ChannelName
	}
	first := create()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/ai/trickle/"+first, nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.NotContains(t, s.channels, first)
	second := create()
	require.NotEqual(t, first, second)
	require.Len(t, s.channels, 1)
}

func TestTrickleActiveStreamAndCancellation(t *testing.T) {
	for _, action := range []string{"delete", "reset", "release"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reg, s := reviewServer(t, "http://127.0.0.1:1")
				require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test"}))
				sid, _, _, _, err := reg.Reserve("r")
				require.NoError(t, err)
				s.newChannel("test", "events", "application/octet-stream", "r", sid)
				input, writer := io.Pipe()
				defer writer.Close()
				pubDone, subDone := make(chan struct{}), make(chan struct{})
				go func() {
					s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/ai/trickle/test/0", input))
					close(pubDone)
				}()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				out := &reviewStreamingWriter{header: http.Header{}, wrote: make(chan struct{}, 1)}
				go func() {
					s.ServeHTTP(out, httptest.NewRequest("GET", "/ai/trickle/test/0", nil).WithContext(ctx))
					close(subDone)
				}()
				_, err = writer.Write([]byte("first"))
				require.NoError(t, err)
				synctest.Wait()
				<-out.wrote
				time.Sleep(time.Minute)
				_, err = writer.Write([]byte("still streaming"))
				require.NoError(t, err)
				synctest.Wait()
				select {
				case <-out.wrote:
				default:
					t.Fatal("active stream timed out")
				}
				switch action {
				case "delete":
					s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("DELETE", "/ai/trickle/test/0", nil))
				case "reset":
					req := httptest.NewRequest("POST", "/ai/trickle/test/1", strings.NewReader("next"))
					req.Header.Set("Lp-Trickle-Reset", "true")
					s.ServeHTTP(httptest.NewRecorder(), req)
				case "release":
					reg.ReleaseBySession(sid)
				}
				synctest.Wait()
				select {
				case <-pubDone:
				default:
					t.Error("publisher was not interrupted")
				}
				select {
				case <-subDone:
				default:
					t.Error("subscriber was not interrupted")
				}
			})
		})
	}
}

func TestNamedChannelsAreIdempotentAndReleased(t *testing.T) {
	reg, s := reviewServer(t, "http://127.0.0.1:1")
	require.NoError(t, reg.AddStatic(StaticRunner{ID: "r", RunnerURL: "http://127.0.0.1:1", App: "test"}))
	sid, _, _, _, err := reg.Reserve("r")
	require.NoError(t, err)
	_, token, _, err := reg.sessionTarget("r", sid)
	require.NoError(t, err)
	var previous string
	for range 2 {
		req := httptest.NewRequest("POST", "/runner/r/session/"+sid+"/channels", strings.NewReader(`{"channels":[{"name":"events"}]}`))
		req.Header.Set("Livepeer-Session-Token", token)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), "application/octet-stream")
		if previous != "" {
			require.Equal(t, previous, w.Body.String())
		}
		previous = w.Body.String()
	}
	require.Len(t, s.channels, 1)
	reg.ReleaseBySession(sid)
	require.Empty(t, s.channels)
}

func TestTrickleLargeSegmentDelivered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, s := reviewServer(t, "http://127.0.0.1:1")
		s.newChannel("large", "events", "application/octet-stream", "", "")
		input, writer := io.Pipe()
		defer writer.Close()
		pub, sub := httptest.NewRecorder(), httptest.NewRecorder()
		pubDone, subDone := make(chan struct{}), make(chan struct{})
		go func() { s.ServeHTTP(pub, httptest.NewRequest("POST", "/ai/trickle/large/0", input)); close(pubDone) }()
		go func() { s.ServeHTTP(sub, httptest.NewRequest("GET", "/ai/trickle/large/0", nil)); close(subDone) }()
		chunk := strings.Repeat("x", 32<<10)
		for range (3 << 20) / len(chunk) {
			_, err := io.WriteString(writer, chunk)
			require.NoError(t, err)
			synctest.Wait() // A reader keeping up must receive the full segment.
		}
		require.NoError(t, writer.Close())
		<-pubDone
		<-subDone
		require.Equal(t, http.StatusOK, pub.Code)
		require.Equal(t, 3<<20, sub.Body.Len())
		late := httptest.NewRecorder()
		s.ServeHTTP(late, httptest.NewRequest("GET", "/ai/trickle/large/0", nil))
		require.Equal(t, http.StatusOK, late.Code)
		require.Equal(t, 3<<20, late.Body.Len())
	})
}
