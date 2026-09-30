package signer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/pm/wire"
)

type maxPrice struct {
	Price    json.Number `json:"price"`
	Currency string      `json:"currency"`
	Unit     string      `json:"unit"`
}

func (s *Service) SetAuthWebhook(endpoint string, grants []string, caFile string, headers map[string]string) error {
	if endpoint == "" {
		return nil
	}
	u, err := destination.ValidateURL(endpoint)
	if err != nil || u.User != nil || u.Fragment != "" {
		return errors.New("invalid signer auth webhook URL")
	}
	policy, err := destination.New("signer-auth-webhook", grants)
	if err != nil {
		return err
	}
	policy, err = policy.WithCAFile(caFile)
	if err != nil {
		return err
	}
	s.authClient = policy.Client()
	s.authClient.Timeout = 5 * time.Second
	// Authentication is one explicit call, including when a target redirects.
	s.authClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s.authURL, s.authHeaders = endpoint, headers
	encoded, _ := json.Marshal(struct {
		URL     string
		Headers map[string]string
	}{endpoint, headers})
	digest := sha256.Sum256(encoded)
	s.authPolicy = hex.EncodeToString(digest[:])
	return nil
}

func (s *Service) authorizePayment(r *http.Request, req paymentRequest, price wire.PriceInfo, state *paymentState) error {
	identity := r.Header.Get("Signer-Auth-Id")
	if identity != "" && state.AuthID != "" && state.AuthID != identity {
		return paymentFailure{403, "signer auth ID changed"}
	}
	if s.authURL != "" && (state.AuthPolicy != s.authPolicy || state.AuthExpiry <= time.Now().Unix()) {
		body, err := json.Marshal(struct {
			Headers http.Header   `json:"headers"`
			State   *paymentState `json:"state"`
		}{r.Header, state})
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.authURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		for name, value := range s.authHeaders {
			request.Header.Set(name, value)
		}
		response, err := s.authClient.Do(request)
		if err != nil {
			return paymentFailure{502, "signer auth webhook unavailable"}
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return paymentFailure{502, "signer auth webhook failed"}
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		var auth struct {
			Status   int       `json:"status"`
			Reason   string    `json:"reason"`
			Expiry   int64     `json:"expiry"`
			AuthID   string    `json:"auth_id"`
			MaxPrice *maxPrice `json:"maxPrice"`
		}
		if err != nil || len(data) > 1<<20 || json.Unmarshal(data, &auth) != nil || (auth.Status != 200 && (auth.Status < 400 || auth.Status > 599)) {
			return paymentFailure{502, "invalid signer auth response"}
		}
		if auth.Status != 200 {
			return paymentFailure{auth.Status, auth.Reason}
		}
		if auth.AuthID != "" {
			identity = auth.AuthID
		}
		if identity != "" && state.AuthID != "" && identity != state.AuthID {
			return paymentFailure{403, "signer auth ID changed"}
		}
		state.AuthExpiry, state.AuthPolicy, state.AuthMaxPrice = auth.Expiry, s.authPolicy, ""
		if auth.MaxPrice != nil {
			if err := checkMaxPrice(paymentRequest{Type: req.Type, MaxPrice: auth.MaxPrice}, price); err != nil {
				return err
			}
			state.AuthMaxPrice = auth.MaxPrice.Price.String()
		}
	}
	if identity != "" {
		state.AuthID = identity
	}
	if s.authURL != "" && state.AuthMaxPrice != "" {
		return checkMaxPrice(paymentRequest{Type: req.Type, MaxPrice: &maxPrice{Price: json.Number(state.AuthMaxPrice), Currency: "wei", Unit: map[string]string{"live": "seconds", "fixed": "fixed"}[req.Type]}}, price)
	}
	return nil
}
