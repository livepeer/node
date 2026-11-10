package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHeartbeatSessionTokenSnapshots(t *testing.T) {
	registry, server := reviewServer(t, "http://127.0.0.1:1")
	defer server.Close()
	app := registeredApp("a")
	app.Capacity = 3
	w := heartbeatHTTP(t, server, map[string]any{"runner_id": "owner", "seq": 1, "apps": []appRequest{app}}, "bootstrap")
	if w.Code != 200 {
		t.Fatal("bootstrap failed")
	}
	var first heartbeatResponse
	if json.Unmarshal(w.Body.Bytes(), &first) != nil {
		t.Fatal("bad response")
	}
	w = heartbeatHTTP(t, server, map[string]any{"runner_id": "owner", "seq": 2, "apps": []appRequest{app}}, first.HeartbeatSecret)
	if w.Code != 200 {
		t.Fatal("confirmation failed")
	}
	active, _, _, status, err := registry.Reserve("a")
	if err != nil || status != 200 {
		t.Fatal("reserve failed")
	}
	pending, _, _, status, err := registry.Reserve("a")
	if err != nil || status != 200 {
		t.Fatal("reserve failed")
	}
	registry.mu.Lock()
	registry.apps["a"].Sessions[pending].pending = true
	token := registry.apps["a"].Sessions[active].Token
	registry.mu.Unlock()
	body := map[string]any{"runner_id": "owner", "seq": 3, "include_session_tokens": true, "apps": []appRequest{app}}
	forbidden := heartbeatHTTP(t, server, body, "wrong")
	if forbidden.Code != 401 || strings.Contains(forbidden.Body.String(), token) {
		t.Fatal("owner authorization bypass")
	}
	w = heartbeatHTTP(t, server, body, first.HeartbeatSecret)
	var got heartbeatResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatal("snapshot failed")
	}
	if len(got.Apps) != 1 || got.Apps[0].SessionTokens == nil {
		t.Fatal("explicit token map missing")
	}
	tokens := *got.Apps[0].SessionTokens
	if tokens[active] != token || tokens[pending] != "" || len(tokens) != 1 {
		t.Fatal("pending session authorized or active token lost")
	}
	discovery := httptest.NewRecorder()
	server.ServeHTTP(discovery, httptest.NewRequest(http.MethodGet, "/discovery", nil))
	if strings.Contains(discovery.Body.String(), token) || strings.Contains(discovery.Body.String(), "session_tokens") {
		t.Fatal("capability leaked into discovery")
	}
	body["seq"] = 4
	delete(body, "include_session_tokens")
	ordinary := heartbeatHTTP(t, server, body, first.HeartbeatSecret)
	if ordinary.Code != 200 || strings.Contains(ordinary.Body.String(), "session_tokens") {
		t.Fatal("unrequested session capabilities returned")
	}
	registry.ReleaseBySession(active)
	registry.ReleaseBySession(pending)
	body["seq"] = 5
	body["include_session_tokens"] = true
	empty := heartbeatHTTP(t, server, body, first.HeartbeatSecret)
	var cleared heartbeatResponse
	if empty.Code != 200 || json.Unmarshal(empty.Body.Bytes(), &cleared) != nil || len(cleared.Apps) != 1 || cleared.Apps[0].SessionTokens == nil || len(*cleared.Apps[0].SessionTokens) != 0 {
		t.Fatal("empty snapshot must explicitly advertise token support")
	}
}
