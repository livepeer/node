package signer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	s.authURL, s.authHeaders = endpoint.Clone(), Headers(http.Header(headers).Clone())
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
	if s.authURL != nil && (state.AuthPolicy != s.authPolicy || state.AuthExpiry == 0 || time.Now().Unix() > state.AuthExpiry) {
		body, err := json.Marshal(struct {
			Headers http.Header   `json:"headers"`
			State   *paymentState `json:"state"`
		}{r.Header, state})
		if err != nil {
			slog.ErrorContext(r.Context(), "signer auth payload encoding failed", "error", err)
			return err
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.authURL.String(), bytes.NewReader(body))
		if err != nil {
			slog.ErrorContext(r.Context(), "signer auth request construction failed", "error", err)
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
			slog.ErrorContext(r.Context(), "signer auth webhook unavailable", "error", err)
			return paymentFailure{502, "signer auth webhook unavailable"}
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			slog.ErrorContext(r.Context(), "signer auth webhook returned non-200 HTTP status", "http_status", response.StatusCode)
			return paymentFailure{502, "signer auth webhook failed"}
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		if err != nil {
			slog.ErrorContext(r.Context(), "signer auth response read failed", "error", err)
			return paymentFailure{502, "invalid signer auth response"}
		}
		if len(data) > 1<<20 {
			slog.ErrorContext(r.Context(), "signer auth response exceeds size limit")
			return paymentFailure{502, "invalid signer auth response"}
		}
		var auth struct {
			Status   int       `json:"status"`
			Reason   string    `json:"reason"`
			Expiry   int64     `json:"expiry"`
			AuthID   string    `json:"auth_id"`
			MaxPrice *maxPrice `json:"maxPrice"`
		}
		if err := json.Unmarshal(data, &auth); err != nil {
			slog.ErrorContext(r.Context(), "signer auth response decoding failed", "error", err)
			return paymentFailure{502, "invalid signer auth response"}
		}
		if auth.Status != 200 && (auth.Status < 400 || auth.Status > 599) {
			slog.ErrorContext(r.Context(), "signer auth response has invalid decision status", "decision_status", auth.Status)
			return paymentFailure{502, "invalid signer auth response"}
		}
		if auth.Status != 200 {
			slog.WarnContext(r.Context(), "signer authorization rejected", "decision_status", auth.Status)
			return paymentFailure{auth.Status, fmt.Sprintf("signer auth rejected request with status %d", auth.Status)}
		}
		if auth.AuthID != "" {
			identity = auth.AuthID
		}
		state.AuthExpiry, state.AuthPolicy, state.AuthMaxPrice = auth.Expiry, s.authPolicy, ""
		if auth.MaxPrice != nil {
			if err := checkMaxPrice(paymentRequest{Type: req.Type, MaxPrice: auth.MaxPrice}, price); err != nil {
				slog.WarnContext(r.Context(), "signer auth price ceiling rejected", "error", err)
				if f, ok := errors.AsType[paymentFailure](err); ok && f.status == http.StatusBadRequest {
					return paymentFailure{http.StatusBadGateway, "signer auth invalid maxPrice"}
				}
				return err
			}
			state.AuthMaxPrice = auth.MaxPrice.Price.String()
		}
	}
	// Apply webhook identity precedence before checking for an established ID change.
	if identity != "" && state.AuthID != identity {
		if state.AuthID != "" {
			slog.WarnContext(r.Context(), "signer authorization identity changed")
			return paymentFailure{403, "signer auth ID changed"}
		}
		state.AuthID = identity
	}
	if s.authURL != nil && state.AuthMaxPrice != "" {
		return checkMaxPrice(paymentRequest{Type: req.Type, MaxPrice: &maxPrice{Price: json.Number(state.AuthMaxPrice), Currency: "wei", Unit: map[string]string{"live": "seconds", "fixed": "fixed"}[req.Type]}}, price)
	}
	return nil
}
