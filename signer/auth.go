package signer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/pm/wire"
)

type maxPrice struct {
	Price    json.Number `json:"price"`
	Currency string      `json:"currency"`
	Unit     string      `json:"unit"`
}

func validateAuthWebhook(endpoint *url.URL) error {
	if err := destination.ValidateURL(endpoint); err != nil {
		return errors.New("invalid signer auth webhook URL")
	}
	if endpoint.User != nil || endpoint.Fragment != "" {
		return errors.New("invalid signer auth webhook URL")
	}
	return nil
}

func (s *Service) SetAuthWebhook(endpoint *url.URL, headers Headers) error {
	if err := validateAuthWebhook(endpoint); err != nil {
		return err
	}
	if _, err := headers.MarshalText(); err != nil {
		return err
	}
	s.authClient = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), Timeout: 5 * time.Second}
	// Authentication is one explicit call, including when a target redirects.
	s.authClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copyURL := *endpoint
	s.authURL, s.authHeaders = &copyURL, Headers(http.Header(headers).Clone())
	encoded, _ := json.Marshal(struct {
		URL     string
		Headers Headers
	}{endpoint.String(), s.authHeaders})
	digest := sha256.Sum256(encoded)
	s.authPolicy = hex.EncodeToString(digest[:])
	return nil
}

func (s *Service) authorizePayment(r *http.Request, req paymentRequest, price wire.PriceInfo, state *paymentState) error {
	identity := r.Header.Get("Signer-Auth-Id")
	if identity != "" && state.AuthID != "" && state.AuthID != identity {
		return paymentFailure{403, "signer auth ID changed"}
	}
	if s.authURL != nil && (state.AuthPolicy != s.authPolicy || state.AuthExpiry <= time.Now().Unix()) {
		body, err := json.Marshal(struct {
			Headers http.Header   `json:"headers"`
			State   *paymentState `json:"state"`
		}{r.Header, state})
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.authURL.String(), bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		for name, values := range s.authHeaders {
			request.Header.Del(name)
			for _, value := range values {
				request.Header.Add(name, value)
			}
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
	if s.authURL != nil && state.AuthMaxPrice != "" {
		return checkMaxPrice(paymentRequest{Type: req.Type, MaxPrice: &maxPrice{Price: json.Number(state.AuthMaxPrice), Currency: "wei", Unit: map[string]string{"live": "seconds", "fixed": "fixed"}[req.Type]}}, price)
	}
	return nil
}
