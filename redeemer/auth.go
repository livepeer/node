package redeemer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/livepeer/node/internal/observability"
	"github.com/livepeer/node/nodeconfig"
	"github.com/livepeer/node/redemption"
)

type authWebhook struct {
	endpoint string
	headers  http.Header
	client   *http.Client
	logger   *slog.Logger
	metrics  *observability.Metrics
}

func newAuthWebhook(endpoint *url.URL, headers nodeconfig.Headers, logger *slog.Logger, metrics *observability.Metrics) *authWebhook {
	return &authWebhook{
		endpoint: endpoint.String(),
		headers:  http.Header(headers).Clone(),
		client: &http.Client{
			Transport:     http.DefaultTransport.(*http.Transport).Clone(),
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		logger:  logger.With("event", "authorization", "operation", "authorization"),
		metrics: metrics,
	}
}

func (a *authWebhook) authorize(w http.ResponseWriter, r *http.Request, ticket *redemption.TicketJSON) bool {
	started := time.Now()
	status, outcome := 0, "error"
	defer func() {
		a.metrics.Add("redeemer_auth_webhook_requests_total", 1, observability.Labels{"status": strconv.Itoa(status), "outcome": outcome})
		a.metrics.Observe("redeemer_auth_webhook_duration_seconds", time.Since(started).Seconds(), nil)
	}()
	fail := func(message, kind string) bool {
		a.logger.ErrorContext(r.Context(), message, "outcome", "error", "error_kind", kind)
		http.Error(w, message, http.StatusBadGateway)
		return false
	}
	type webhookTicket struct {
		*redemption.TicketJSON
		// Shadow the JSON field without changing the ticket used for redemption.
		Randomness *string `json:"recipient_rand,omitempty"`
	}
	var outgoing *webhookTicket
	if ticket != nil {
		outgoing = &webhookTicket{TicketJSON: ticket}
	}
	body, err := json.Marshal(struct {
		Headers http.Header    `json:"headers"`
		Ticket  *webhookTicket `json:"ticket"`
	}{r.Header, outgoing})
	if err != nil {
		return fail("redeemer auth payload encoding failed", observability.ErrorKind(err))
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return fail("redeemer auth request construction failed", observability.ErrorKind(err))
	}
	// Prevent transport retries, including with a configured idempotency key.
	request.GetBody = nil
	request.Header = a.headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return fail("redeemer auth webhook unavailable", observability.ErrorKind(err))
	}
	defer response.Body.Close()
	status = response.StatusCode
	if status != http.StatusOK {
		return fail("redeemer auth webhook failed", "http_status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return fail("invalid redeemer auth response", observability.ErrorKind(err))
	}
	if len(data) > 1<<20 {
		return fail("invalid redeemer auth response", "response_limit")
	}
	var decision struct {
		Status int    `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &decision); err != nil {
		return fail("invalid redeemer auth response", observability.ErrorKind(err))
	}
	if decision.Status == http.StatusOK {
		outcome = "success"
		return true
	}
	if decision.Status < 400 || decision.Status > 599 {
		return fail("invalid redeemer auth response", "invalid_decision")
	}
	outcome = "denied"
	a.logger.WarnContext(r.Context(), "redeemer authorization rejected", "outcome", outcome, "error_kind", "policy_rejected", "decision_status", decision.Status)
	deniedStatus := decision.Status
	if deniedStatus == http.StatusConflict {
		// Forwarders permanently reject 409 as conflicting ticket metadata.
		deniedStatus = http.StatusForbidden
	}
	http.Error(w, fmt.Sprintf("redeemer auth rejected request with status %d", decision.Status), deniedStatus)
	return false
}
