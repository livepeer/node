package orchestrator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/livepeer/node/internal/destination"
)

const (
	maxControlBody     = 1 << 20
	maxTricklePart     = 8 << 20
	maxTrickleBytes    = 64 << 20
	maxTrickleChannels = 1024
	maxTrickleParts    = 64
)

type channel struct {
	name      string
	mime      string
	runnerID  string
	sessionID string
	parts     map[int][]byte
	latest    int
	closed    bool
	wakeup    chan struct{}
}

type Server struct {
	registry     *Registry
	runnerPolicy destination.Policy
	proxyPolicy  destination.Policy
	logger       *slog.Logger
	mux          *http.ServeMux
	mu           sync.Mutex
	channels     map[string]*channel
	o2r          map[string]string
	bytesUsed    int
}

func NewServer(registry *Registry, runnerPolicy, proxyPolicy destination.Policy, logger *slog.Logger) *Server {
	s := &Server{registry: registry, runnerPolicy: runnerPolicy, proxyPolicy: proxyPolicy, logger: logger, mux: http.NewServeMux(), channels: map[string]*channel{}, o2r: map[string]string{}}
	registry.onEvent = s.emitSessionEvent
	s.mux.HandleFunc("POST /runners/heartbeat", s.heartbeat)
	s.mux.HandleFunc("POST /runners/{runner_id}/unregister", s.unregister)
	s.mux.HandleFunc("GET /discovery", s.discovery)
	s.mux.HandleFunc("POST /apps/{runner_id}/session", s.reserve)
	s.mux.HandleFunc("POST /apps/{runner_id}/session/{session_id}/stop", s.clientStop)
	s.mux.HandleFunc("POST /runner/{runner_id}/session/{session_id}/stop", s.runnerStop)
	s.mux.HandleFunc("POST /runner/{runner_id}/session/{session_id}/proxy", s.createProxy)
	s.mux.HandleFunc("POST /runner/{runner_id}/session/{session_id}/channels", s.createChannels)
	s.mux.HandleFunc("DELETE /runner/{runner_id}/session/{session_id}/channels", s.deleteChannels)
	s.mux.HandleFunc("/apps/{runner_id}/session/{session_id}/app", s.proxySession)
	s.mux.HandleFunc("/apps/{runner_id}/session/{session_id}/app/{app_path...}", s.proxySession)
	s.mux.HandleFunc("/apps/{runner_id}/app", s.proxySingleShot)
	s.mux.HandleFunc("/apps/{runner_id}/app/{app_path...}", s.proxySingleShot)
	s.mux.HandleFunc("/proxy/{proxy_id}", s.proxyGenerated)
	s.mux.HandleFunc("/proxy/{proxy_id}/{app_path...}", s.proxyGenerated)
	s.mux.HandleFunc("/ai/trickle/{channel}", s.trickle)
	s.mux.HandleFunc("/ai/trickle/{channel}/{seq}", s.trickle)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decode(r *http.Request, value any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxControlBody))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if err := decode(r, &req); err != nil {
		fail(w, http.StatusBadRequest, "invalid heartbeat JSON")
		return
	}
	resp, status, err := s.registry.Heartbeat(req, r.Header.Get("Authorization"))
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	if resp.HeartbeatSecret != "" {
		id, err := randomID()
		if err != nil {
			fail(w, http.StatusInternalServerError, "runner channel creation failed")
			return
		}
		s.mu.Lock()
		s.channels[id] = &channel{name: "o2r", mime: "application/json", runnerID: resp.RunnerID, parts: map[int][]byte{}, latest: -1, wakeup: make(chan struct{})}
		s.o2r[resp.RunnerID] = id
		s.mu.Unlock()
		address := s.registry.service + "/ai/trickle/" + id
		resp.O2R = &trickleChannel{Name: "o2r", ChannelName: id, URL: address, InternalURL: address, MimeType: "application/json"}
	}
	writeJSON(w, status, resp)
}

func (s *Server) emitSessionEvent(runnerID, event, sid string) {
	if event == "expired" || event == "unregistered" {
		s.mu.Lock()
		for id, ch := range s.channels {
			if ch.runnerID == runnerID {
				s.closeChannel(id, ch)
			}
		}
		delete(s.o2r, runnerID)
		s.mu.Unlock()
		return
	}
	data, err := json.Marshal(map[string]string{"event": event, "session": sid, "timestamp": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.channels[s.o2r[runnerID]]
	if ch == nil || ch.closed {
		return
	}
	_ = s.storePart(ch, ch.latest+1, data)
	if event == "released" {
		for id, owned := range s.channels {
			if owned.runnerID == runnerID && owned.sessionID == sid && !owned.closed {
				s.closeChannel(id, owned)
			}
		}
	}
}

// storePart and closeChannel require s.mu.
func (s *Server) storePart(ch *channel, seq int, data []byte) bool {
	if existing, exists := ch.parts[seq]; exists {
		return bytes.Equal(existing, data)
	}
	oldest, oldSize := 0, 0
	if len(ch.parts) >= maxTrickleParts {
		first := true
		for index := range ch.parts {
			if first || index < oldest {
				oldest = index
				first = false
			}
		}
		oldSize = len(ch.parts[oldest])
	}
	if s.bytesUsed-oldSize+len(data) > maxTrickleBytes {
		return false
	}
	if len(ch.parts) >= maxTrickleParts {
		s.bytesUsed -= oldSize
		delete(ch.parts, oldest)
	}
	ch.parts[seq] = data
	s.bytesUsed += len(data)
	if seq > ch.latest {
		ch.latest = seq
	}
	close(ch.wakeup)
	ch.wakeup = make(chan struct{})
	return true
}

func (s *Server) closeChannel(id string, ch *channel) {
	ch.closed = true
	close(ch.wakeup)
	ch.wakeup = make(chan struct{})
	for _, part := range ch.parts {
		s.bytesUsed -= len(part)
	}
	ch.parts = nil
	delete(s.channels, id)
}

func (s *Server) unregister(w http.ResponseWriter, r *http.Request) {
	status, err := s.registry.Unregister(r.PathValue("runner_id"), r.Header.Get("Authorization"))
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.registry.Discovery())
}

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	id, appURL, controlURL, status, err := s.registry.Reserve(r.PathValue("runner_id"))
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"session_id": id, "app_url": appURL, "control_url": controlURL})
}

func (s *Server) clientStop(w http.ResponseWriter, r *http.Request) {
	status, err := s.registry.release(r.PathValue("runner_id"), r.PathValue("session_id"), "", false)
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) runnerStop(w http.ResponseWriter, r *http.Request) {
	status, err := s.registry.release(r.PathValue("runner_id"), r.PathValue("session_id"), r.Header.Get("Livepeer-Session-Token"), true)
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) callbackAuthorized(r *http.Request) bool {
	return s.registry.validSessionToken(r.PathValue("runner_id"), r.PathValue("session_id"), r.Header.Get("Livepeer-Session-Token"))
}

func (s *Server) createProxy(w http.ResponseWriter, r *http.Request) {
	if !s.callbackAuthorized(r) {
		fail(w, http.StatusForbidden, "invalid session token")
		return
	}
	var body struct {
		TargetURL string `json:"target_url"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid proxy JSON")
		return
	}
	target, err := destination.ValidateURL(body.TargetURL)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.registry.addProxy(r.PathValue("runner_id"), r.PathValue("session_id"), r.Header.Get("Livepeer-Session-Token"), target)
	if err != nil {
		fail(w, http.StatusForbidden, "invalid session token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"proxy_id": id, "url": s.registry.service + "/proxy/" + id})
}

func (s *Server) proxySession(w http.ResponseWriter, r *http.Request) {
	target, token, status, err := s.registry.sessionTarget(r.PathValue("runner_id"), r.PathValue("session_id"))
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	control := s.registry.service + "/runner/" + url.PathEscape(r.PathValue("runner_id")) + "/session/" + url.PathEscape(r.PathValue("session_id"))
	s.proxy(w, r, s.runnerPolicy, target, r.PathValue("app_path"), r.PathValue("runner_id"), r.PathValue("session_id"), token, control)
}

func (s *Server) proxySingleShot(w http.ResponseWriter, r *http.Request) {
	target, status, err := s.registry.singleShotTarget(r.PathValue("runner_id"))
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	s.proxy(w, r, s.runnerPolicy, target, r.PathValue("app_path"), r.PathValue("runner_id"), "", "", "")
}

func (s *Server) proxyGenerated(w http.ResponseWriter, r *http.Request) {
	target, runnerID, sid, token, ok := s.registry.proxyTarget(r.PathValue("proxy_id"))
	if !ok {
		fail(w, http.StatusNotFound, "proxy not found")
		return
	}
	control := s.registry.service + "/runner/" + url.PathEscape(runnerID) + "/session/" + url.PathEscape(sid)
	s.proxy(w, r, s.proxyPolicy, target, r.PathValue("app_path"), runnerID, sid, token, control)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, policy destination.Policy, target *url.URL, appPath, runnerID, sid, token, control string) {
	proxy := &httputil.ReverseProxy{
		Transport:     policy.Transport(0),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			s.logger.Warn("runner proxy failed", "runner_id", runnerID, "session_id", sid, "error_kind", "upstream_failure")
			fail(w, http.StatusBadGateway, "runner destination unavailable")
		},
		Rewrite: func(req *httputil.ProxyRequest) {
			u := *target
			u.RawQuery = target.RawQuery
			if req.In.URL.RawQuery != "" {
				if u.RawQuery != "" {
					u.RawQuery += "&"
				}
				u.RawQuery += req.In.URL.RawQuery
			}
			req.SetURL(&u)
			req.Out.URL.Path = strings.TrimRight(target.Path, "/") + "/" + strings.TrimLeft(appPath, "/")
			req.Out.URL.RawPath = ""
			req.Out.URL.RawQuery = u.RawQuery
			req.SetXForwarded()
			req.Out.Header.Set("Livepeer-Runner-Route", runnerID)
			if sid != "" {
				req.Out.Header.Set("Livepeer-Session-Id", sid)
				req.Out.Header.Set("Livepeer-Session-Token", token)
				req.Out.Header.Set("Livepeer-Session-Control", control)
			}
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) createChannels(w http.ResponseWriter, r *http.Request) {
	if !s.callbackAuthorized(r) {
		fail(w, http.StatusForbidden, "invalid session token")
		return
	}
	var body struct {
		Channels []struct {
			Name     string `json:"name"`
			MimeType string `json:"mime_type"`
		} `json:"channels"`
	}
	if err := decode(r, &body); err != nil || len(body.Channels) > 25 {
		fail(w, http.StatusBadRequest, "invalid channels request")
		return
	}
	result := []map[string]string{}
	for _, item := range body.Channels {
		if item.Name == "" || item.MimeType == "" {
			fail(w, http.StatusBadRequest, "name and mime_type required")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Reserve space for one O2R channel per possible runner.
	if len(s.channels)+len(body.Channels) > maxTrickleChannels-maxRunners {
		fail(w, http.StatusServiceUnavailable, "too many channels")
		return
	}
	for _, item := range body.Channels {
		id, err := randomID()
		if err != nil {
			fail(w, http.StatusInternalServerError, "channel creation failed")
			return
		}
		s.channels[id] = &channel{name: item.Name, mime: item.MimeType, runnerID: r.PathValue("runner_id"), sessionID: r.PathValue("session_id"), parts: map[int][]byte{}, latest: -1, wakeup: make(chan struct{})}
		address := s.registry.service + "/ai/trickle/" + id
		result = append(result, map[string]string{"name": item.Name, "channel_name": id, "url": address, "internal_url": address, "mime_type": item.MimeType})
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": result})
}

func (s *Server) deleteChannels(w http.ResponseWriter, r *http.Request) {
	if !s.callbackAuthorized(r) {
		fail(w, http.StatusForbidden, "invalid session token")
		return
	}
	var body struct {
		Channels []string `json:"channels"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "invalid channels request")
		return
	}
	deleted := []string{}
	s.mu.Lock()
	for _, name := range body.Channels {
		for id, ch := range s.channels {
			if ch.runnerID == r.PathValue("runner_id") && ch.sessionID == r.PathValue("session_id") && (id == name || ch.name == name) {
				s.closeChannel(id, ch)
				deleted = append(deleted, name)
			}
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) trickle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("channel")
	s.mu.Lock()
	ch := s.channels[id]
	s.mu.Unlock()
	if ch == nil {
		fail(w, http.StatusNotFound, "channel not found")
		return
	}
	if r.Method == http.MethodDelete {
		if r.PathValue("seq") != "" {
			seq, err := strconv.Atoi(r.PathValue("seq"))
			if err != nil || seq < 0 {
				fail(w, http.StatusBadRequest, "invalid sequence")
				return
			}
			s.mu.Lock()
			_, exists := ch.parts[seq]
			s.mu.Unlock()
			if !exists {
				fail(w, http.StatusBadRequest, "segment not found")
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		s.mu.Lock()
		s.closeChannel(id, ch)
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if r.PathValue("seq") == "" {
		if r.Method == http.MethodPost {
			// Channels are created only through the authenticated session
			// callback. The public publisher create call is idempotent.
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.PathValue("seq") == "next" && r.Method == http.MethodGet {
		s.mu.Lock()
		next := ch.latest + 1
		closed := ch.closed
		s.mu.Unlock()
		w.Header().Set("Lp-Trickle-Latest", strconv.Itoa(next))
		if closed {
			w.Header().Set("Lp-Trickle-Closed", "terminated")
		}
		_, _ = io.WriteString(w, strconv.Itoa(next))
		return
	}
	seq, err := strconv.Atoi(r.PathValue("seq"))
	if err != nil {
		fail(w, http.StatusBadRequest, "sequence required")
		return
	}
	if r.Method == http.MethodPost {
		if seq < 0 {
			fail(w, http.StatusBadRequest, "invalid sequence")
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxTricklePart+1))
		if err != nil || len(data) > maxTricklePart {
			fail(w, http.StatusRequestEntityTooLarge, "trickle segment too large")
			return
		}
		s.mu.Lock()
		if ch.closed {
			s.mu.Unlock()
			w.Header().Set("Lp-Trickle-Closed", "true")
			fail(w, http.StatusGone, "channel closed")
			return
		}
		if !s.storePart(ch, seq, data) {
			s.mu.Unlock()
			fail(w, http.StatusServiceUnavailable, "trickle buffer full or duplicate sequence")
			return
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	for {
		s.mu.Lock()
		requested := seq
		if requested == -2 {
			requested = ch.latest
		}
		if requested == -1 {
			requested = ch.latest + 1
		}
		data, ok := ch.parts[requested]
		closed := ch.closed
		latest := ch.latest
		wakeup := ch.wakeup
		s.mu.Unlock()
		if ok {
			w.Header().Set("Content-Type", ch.mime)
			w.Header().Set("Lp-Trickle-Seq", strconv.Itoa(requested))
			w.Header().Set("Lp-Trickle-Latest", strconv.Itoa(latest))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		if requested < latest-maxTrickleParts+1 && !ok {
			w.Header().Set("Lp-Trickle-Latest", strconv.Itoa(latest))
			w.WriteHeader(470)
			return
		}
		if closed {
			w.Header().Set("Lp-Trickle-Closed", "true")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		select {
		case <-wakeup:
		case <-r.Context().Done():
			return
		case <-time.After(25 * time.Second):
			w.Header().Set("Lp-Trickle-Latest", strconv.Itoa(latest))
			w.WriteHeader(470)
			return
		}
	}
}

func (s *Server) String() string { return fmt.Sprint(s.registry) }
