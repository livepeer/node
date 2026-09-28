package orchestrator

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/livepeer/node/internal/destination"
)

type priceInfo struct {
	Price    json.Number `json:"price" toml:"price"`
	Currency string      `json:"currency" toml:"currency"`
	Unit     string      `json:"unit" toml:"unit"`
}

type heartbeatRequest struct {
	RunnerID   string    `json:"runner_id"`
	Label      string    `json:"label"`
	RunnerURL  string    `json:"runner_url"`
	Version    string    `json:"version"`
	Metadata   string    `json:"metadata"`
	Status     string    `json:"status"`
	Mode       string    `json:"mode"`
	Proxy      bool      `json:"proxy"`
	App        string    `json:"app"`
	Capacity   int       `json:"capacity"`
	PriceInfo  priceInfo `json:"price_info"`
	SessionIDs []string  `json:"session_ids"`
}

type heartbeatResponse struct {
	RunnerID          string          `json:"runner_id"`
	Orchestrator      string          `json:"orchestrator,omitempty"`
	HeartbeatInterval string          `json:"heartbeat_interval"`
	HeartbeatTTL      string          `json:"heartbeat_ttl"`
	HeartbeatSecret   string          `json:"heartbeat_secret,omitempty"`
	SessionIDs        []string        `json:"session_ids"`
	O2R               *trickleChannel `json:"o2r,omitempty"`
}

type trickleChannel struct {
	Name        string `json:"name"`
	ChannelName string `json:"channel_name"`
	URL         string `json:"url"`
	InternalURL string `json:"internal_url,omitempty"`
	MimeType    string `json:"mime_type"`
}

type runner struct {
	heartbeatRequest
	Credential string
	Last       time.Time
	Static     bool
	Healthy    bool
	HealthURL  string
	HealthCode int
	Sessions   map[string]*session
}

type session struct {
	ID      string
	Token   string
	Proxies map[string]*url.URL
	Created time.Time
}

type Registry struct {
	mu       sync.Mutex
	runners  map[string]*runner
	secret   string
	service  string
	interval time.Duration
	ttl      time.Duration
	onEvent  func(runnerID, event, sessionID string)
}

const maxRunners = 256

func NewRegistry(secret, service string, interval, ttl time.Duration) *Registry {
	return &Registry{runners: make(map[string]*runner), secret: secret, service: strings.TrimRight(service, "/"), interval: interval, ttl: ttl}
}

func randomID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func equalSecret(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func validateHeartbeat(req heartbeatRequest) error {
	if req.RunnerID != "" && !validRouteID(req.RunnerID) {
		return errors.New("runner_id must contain only ASCII letters, digits, '-' or '_' and be at most 128 bytes")
	}
	if req.App == "" || req.Capacity < 1 || req.Capacity > 10000 {
		return errors.New("app and capacity between 1 and 10000 are required")
	}
	if _, err := destination.ValidateURL(req.RunnerURL); err != nil {
		return errors.New("runner_url must be an absolute HTTP or HTTPS URL")
	}
	if req.Mode != "" && req.Mode != "persistent" && req.Mode != "single-shot" {
		return errors.New("mode must be persistent or single-shot")
	}
	if req.PriceInfo.Unit == "720p" {
		return errors.New("720p pricing is unsupported")
	}
	if req.PriceInfo.Unit != "" && req.PriceInfo.Unit != "hour" && req.PriceInfo.Unit != "fixed" {
		return errors.New("price_info.unit must be hour or fixed")
	}
	price := req.PriceInfo.Price.String()
	if price != "" && price != "0" {
		return errors.New("paid runners require on-chain payment support")
	}
	return nil
}

func validRouteID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func (r *Registry) Heartbeat(req heartbeatRequest, auth string) (heartbeatResponse, int, error) {
	if err := validateHeartbeat(req); err != nil {
		return heartbeatResponse{}, http.StatusBadRequest, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.runners[req.RunnerID]
	if current != nil && current.Static {
		return heartbeatResponse{}, http.StatusForbidden, errors.New("static runner cannot heartbeat")
	}
	initial := current == nil
	if initial {
		if !equalSecret(auth, r.secret) {
			return heartbeatResponse{}, http.StatusUnauthorized, errors.New("invalid runner credential")
		}
		if len(r.runners) >= maxRunners {
			return heartbeatResponse{}, http.StatusServiceUnavailable, errors.New("runner capacity reached")
		}
		id := req.RunnerID
		if id == "" {
			var err error
			id, err = randomID()
			if err != nil {
				return heartbeatResponse{}, http.StatusInternalServerError, err
			}
		}
		credential, err := randomID()
		if err != nil {
			return heartbeatResponse{}, http.StatusInternalServerError, err
		}
		current = &runner{Credential: credential, Sessions: make(map[string]*session), Healthy: true}
		req.RunnerID = id
		r.runners[id] = current
	} else if !equalSecret(auth, current.Credential) {
		return heartbeatResponse{}, http.StatusUnauthorized, errors.New("invalid runner credential")
	}
	if req.Mode == "" {
		req.Mode = "persistent"
	}
	if req.Status == "" {
		req.Status = "ready"
	}
	current.heartbeatRequest = req
	current.Last = time.Now()
	// The SDK sends its active session IDs for reconciliation. Preserve
	// reservations until an explicit stop or runner expiry; the runner may
	// have missed a callback and will receive the authoritative list.
	ids := make([]string, 0, len(current.Sessions))
	for id := range current.Sessions {
		ids = append(ids, id)
	}
	resp := heartbeatResponse{RunnerID: req.RunnerID, Orchestrator: r.service, HeartbeatInterval: r.interval.String(), HeartbeatTTL: r.ttl.String(), SessionIDs: ids}
	if initial {
		resp.HeartbeatSecret = current.Credential
	}
	return resp, http.StatusOK, nil
}

func (r *Registry) Unregister(id, auth string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.runners[id]
	if current == nil {
		return http.StatusNotFound, errors.New("runner not found")
	}
	if current.Static || !equalSecret(current.Credential, auth) {
		return http.StatusUnauthorized, errors.New("invalid runner credential")
	}
	if r.onEvent != nil {
		r.onEvent(id, "unregistered", "")
	}
	delete(r.runners, id)
	return http.StatusOK, nil
}

type discoveryRunner struct {
	URL               string    `json:"url"`
	App               string    `json:"app"`
	Version           string    `json:"version,omitempty"`
	Metadata          string    `json:"metadata,omitempty"`
	Mode              string    `json:"mode"`
	Capacity          int       `json:"capacity"`
	CapacityUsed      int       `json:"capacity_used"`
	CapacityAvailable int       `json:"capacity_available"`
	PriceInfo         priceInfo `json:"price_info"`
}

type discoveryEntry struct {
	Address string            `json:"address"`
	Runners []discoveryRunner `json:"runners"`
}

func (r *Registry) Discovery() []discoveryEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := []discoveryRunner{}
	for id, item := range r.runners {
		if !r.usable(item) {
			continue
		}
		available := item.Capacity - len(item.Sessions)
		if available <= 0 {
			continue
		}
		result = append(result, discoveryRunner{URL: r.appURL(id, item.Mode, ""), App: item.App, Version: item.Version, Metadata: item.Metadata, Mode: item.Mode, Capacity: item.Capacity, CapacityUsed: len(item.Sessions), CapacityAvailable: available, PriceInfo: item.PriceInfo})
	}
	return []discoveryEntry{{Address: r.service, Runners: result}}
}

func (r *Registry) usable(item *runner) bool {
	return (item.Static || time.Since(item.Last) <= r.ttl) && item.Healthy && item.Status == "ready"
}

func (r *Registry) appURL(id, mode, sessionID string) string {
	if mode == "single-shot" {
		return r.service + "/apps/" + url.PathEscape(id) + "/app"
	}
	if sessionID == "" {
		return r.service + "/apps/" + url.PathEscape(id) + "/session"
	}
	return r.service + "/apps/" + url.PathEscape(id) + "/session/" + url.PathEscape(sessionID) + "/app"
}

func (r *Registry) Reserve(id string) (string, string, string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.runners[id]
	if item == nil || !r.usable(item) {
		return "", "", "", http.StatusNotFound, errors.New("runner unavailable")
	}
	if item.Mode != "persistent" {
		return "", "", "", http.StatusBadRequest, errors.New("only persistent runners have sessions")
	}
	if len(item.Sessions) >= item.Capacity {
		return "", "", "", http.StatusConflict, errors.New("runner at capacity")
	}
	sessionID, err := randomID()
	if err != nil {
		return "", "", "", http.StatusInternalServerError, err
	}
	token, err := randomID()
	if err != nil {
		return "", "", "", http.StatusInternalServerError, err
	}
	item.Sessions[sessionID] = &session{ID: sessionID, Token: token, Created: time.Now(), Proxies: map[string]*url.URL{}}
	if r.onEvent != nil {
		r.onEvent(id, "reserved", sessionID)
	}
	return sessionID, r.appURL(id, item.Mode, sessionID), r.service + "/apps/" + url.PathEscape(id) + "/session/" + url.PathEscape(sessionID), http.StatusOK, nil
}

func (r *Registry) release(id, sid, token string, callback bool) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.runners[id]
	if item == nil || item.Sessions[sid] == nil {
		return http.StatusNotFound, errors.New("session not found")
	}
	if callback && !equalSecret(item.Sessions[sid].Token, token) {
		return http.StatusForbidden, errors.New("invalid session token")
	}
	delete(item.Sessions, sid)
	if r.onEvent != nil {
		r.onEvent(id, "released", sid)
	}
	return http.StatusOK, nil
}

func (r *Registry) sessionTarget(id, sid string) (*url.URL, string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.runners[id]
	if item == nil || !r.usable(item) || item.Sessions[sid] == nil {
		return nil, "", http.StatusNotFound, errors.New("session not found")
	}
	target, err := destination.ValidateURL(item.RunnerURL)
	if err != nil {
		return nil, "", http.StatusBadGateway, err
	}
	return target, item.Sessions[sid].Token, http.StatusOK, nil
}

func (r *Registry) validSessionToken(id, sid, token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.runners[id]
	return item != nil && item.Sessions[sid] != nil && equalSecret(item.Sessions[sid].Token, token)
}

func (r *Registry) addProxy(id, sid, token string, target *url.URL) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.runners[id]
	if item == nil || item.Sessions[sid] == nil || !equalSecret(item.Sessions[sid].Token, token) {
		return "", errors.New("invalid session token")
	}
	proxyID, err := randomID()
	if err != nil {
		return "", err
	}
	item.Sessions[sid].Proxies[proxyID] = target
	return proxyID, nil
}

func (r *Registry) proxyTarget(proxyID string) (*url.URL, string, string, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for runnerID, item := range r.runners {
		if !r.usable(item) {
			continue
		}
		for sid, sess := range item.Sessions {
			if target := sess.Proxies[proxyID]; target != nil {
				return target, runnerID, sid, sess.Token, true
			}
		}
	}
	return nil, "", "", "", false
}

func (r *Registry) singleShotTarget(id string) (*url.URL, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.runners[id]
	if item == nil || !r.usable(item) {
		return nil, http.StatusNotFound, errors.New("runner unavailable")
	}
	if item.Mode != "single-shot" {
		return nil, http.StatusBadRequest, errors.New("runner is not single-shot")
	}
	target, err := destination.ValidateURL(item.RunnerURL)
	if err != nil {
		return nil, http.StatusBadGateway, err
	}
	return target, http.StatusOK, nil
}

func (r *Registry) Expire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, item := range r.runners {
		if !item.Static && time.Since(item.Last) > r.ttl {
			if r.onEvent != nil {
				r.onEvent(id, "expired", "")
			}
			delete(r.runners, id)
		}
	}
}

type StaticRunner struct {
	ID         string `toml:"id"`
	RunnerURL  string `toml:"runner_url"`
	App        string `toml:"app"`
	Mode       string `toml:"mode"`
	Status     string `toml:"status"`
	Capacity   int    `toml:"capacity"`
	HealthURL  string `toml:"health_url"`
	HealthCode int    `toml:"healthy_status_code"`
}

func (r *Registry) AddStatic(config StaticRunner) error {
	if !validRouteID(config.ID) {
		return errors.New("static runner id must be a route-safe identifier")
	}
	req := heartbeatRequest{RunnerID: config.ID, RunnerURL: config.RunnerURL, App: config.App, Mode: config.Mode, Status: config.Status, Capacity: config.Capacity}
	if err := validateHeartbeat(req); err != nil {
		return fmt.Errorf("static runner %s: %w", config.ID, err)
	}
	if config.HealthURL != "" {
		if _, err := destination.ValidateURL(config.HealthURL); err != nil {
			return fmt.Errorf("static runner %s has invalid health URL", config.ID)
		}
	}
	if req.Mode == "" {
		req.Mode = "persistent"
	}
	if req.Status == "" {
		req.Status = "ready"
	}
	if config.HealthCode == 0 {
		config.HealthCode = http.StatusOK
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runners[config.ID] != nil {
		return fmt.Errorf("duplicate static runner id %s", config.ID)
	}
	if len(r.runners) >= maxRunners {
		return errors.New("static runner capacity reached")
	}
	r.runners[config.ID] = &runner{heartbeatRequest: req, Static: true, Healthy: config.HealthURL == "", HealthURL: config.HealthURL, HealthCode: config.HealthCode, Sessions: map[string]*session{}}
	return nil
}

func (r *Registry) CheckStaticHealth(client *http.Client) {
	r.mu.Lock()
	checks := make(map[string]string)
	for id, item := range r.runners {
		if item.Static && item.HealthURL != "" {
			checks[id] = item.HealthURL
		}
	}
	r.mu.Unlock()
	for id, raw := range checks {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			continue
		}
		response, err := client.Do(request)
		healthy := false
		if err == nil {
			_ = response.Body.Close()
			r.mu.Lock()
			item := r.runners[id]
			healthy = item != nil && response.StatusCode == item.HealthCode
			r.mu.Unlock()
		}
		r.mu.Lock()
		if item := r.runners[id]; item != nil && item.Static {
			item.Healthy = healthy
			if !healthy {
				item.Sessions = map[string]*session{}
			}
		}
		r.mu.Unlock()
	}
}

func (r *Registry) Counts() (runners, sessions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.runners {
		runners++
		sessions += len(item.Sessions)
	}
	return
}

func (r *Registry) String() string {
	runners, sessions := r.Counts()
	return fmt.Sprintf("runners=%d sessions=%d", runners, sessions)
}
