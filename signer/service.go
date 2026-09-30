package signer

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/livepeer/node/destination"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/signercompat"
)

type signedState struct {
	State []byte `json:"state"`
	Sig   []byte `json:"sig"`
}
type paymentState struct {
	StateID              string
	PMSessionID          string
	LastUpdate           time.Time
	OrchestratorAddress  ethcommon.Address
	App                  string
	AuthExpiry           int64
	SenderNonce          uint32
	Balance              string
	InitialPricePerUnit  int64
	InitialPixelsPerUnit int64
	Type                 string
	SequenceNumber       uint64
	AuthID               string
	ManifestID           string
}
type paymentRequest struct {
	State        signedState `json:"state,omitempty"`
	Orchestrator []byte      `json:"orchestrator"`
	ManifestID   string
	App          string `json:"app,omitempty"`
	Type         string `json:"type"`
	MaxPrice     *struct {
		Price    json.Number `json:"price"`
		Currency string      `json:"currency"`
		Unit     string      `json:"unit"`
	} `json:"maxPrice,omitempty"`
	Capabilities []byte `json:"capabilities,omitempty"`
}
type paymentResponse struct {
	Payment  string      `json:"payment"`
	SegCreds string      `json:"segCreds,omitempty"`
	State    signedState `json:"state"`
}

type Service struct {
	key             *eth.Key
	store           *stateStore
	authToken       string
	mux             *http.ServeMux
	discoveryURLs   []string
	discoveryClient *http.Client
	paymentChain    pm.SenderChain
	senderPolicy    pm.SenderPolicy
}

func (s *Service) SetPaymentChain(chain pm.SenderChain) { s.paymentChain = chain }

func (s *Service) SetDiscovery(orchestrators, grants []string, caFile string) error {
	policy, err := destination.New("signer-discovery", grants)
	if err != nil {
		return err
	}
	policy, err = policy.WithCAFile(caFile)
	if err != nil {
		return err
	}
	client := policy.Client()
	client.Timeout = 5 * time.Second
	urls := make([]string, 0, len(orchestrators))
	for _, candidate := range orchestrators {
		parsed, err := destination.ValidateURL(candidate)
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("invalid signer discovery orchestrator URL")
		}
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/discovery"
		urls = append(urls, parsed.String())
	}
	s.discoveryClient = client
	s.discoveryURLs = urls
	return nil
}

func OpenService(key *eth.Key, statePath, authToken string) (*Service, error) {
	store, err := openStateStore(statePath)
	if err != nil {
		return nil, err
	}
	return NewService(key, store, authToken), nil
}

func (s *Service) Close() error { return s.store.close() }

func NewService(key *eth.Key, store *stateStore, authToken string) *Service {
	s := &Service{key: key, store: store, authToken: authToken, mux: http.NewServeMux(), senderPolicy: pm.DefaultSenderPolicy()}
	s.mux.HandleFunc("POST /sign-orchestrator-info", s.signInfo)
	s.mux.HandleFunc("POST /generate-live-payment", s.generate)
	s.mux.HandleFunc("GET /discover-orchestrators", s.discover)
	return s
}
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.authToken != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.authToken)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.mux.ServeHTTP(w, r)
}
func signerError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}
func (s *Service) signInfo(w http.ResponseWriter, _ *http.Request) {
	address := s.key.Address().Hex()
	sig, err := s.key.SignMessage([]byte(address))
	if err != nil {
		signerError(w, 500, "signing failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"address": address, "signature": eth.HexSignature(sig)})
}

type discoveredGPU struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	VRAMMB int    `json:"vram_mb,omitempty"`
}

type discoveredRunner struct {
	URL               string         `json:"url"`
	App               string         `json:"app"`
	Version           string         `json:"version,omitempty"`
	Metadata          string         `json:"metadata,omitempty"`
	GPU               *discoveredGPU `json:"gpu,omitempty"`
	Mode              string         `json:"mode"`
	Capacity          int            `json:"capacity"`
	CapacityUsed      int            `json:"capacity_used"`
	CapacityAvailable int            `json:"capacity_available"`
	PriceInfo         struct {
		Price    json.Number `json:"price"`
		PriceUSD json.Number `json:"price_usd,omitempty"`
		Currency string      `json:"currency"`
		Unit     string      `json:"unit"`
	} `json:"price_info"`
}
type discoveredOrchestrator struct {
	Address  string             `json:"address"`
	Score    float32            `json:"score"`
	Runners  []discoveredRunner `json:"runners"`
	LastSeen time.Time          `json:"last_seen"`
}

func (s *Service) discover(w http.ResponseWriter, r *http.Request) {
	if len(s.discoveryURLs) == 0 {
		signerError(w, http.StatusServiceUnavailable, "no orchestrator discovery source configured")
		return
	}
	appFilter := r.URL.Query()["app"]
	gpuFilter := r.URL.Query()["gpu"]
	result := make([]discoveredOrchestrator, 0, len(s.discoveryURLs))
	for _, endpoint := range s.discoveryURLs {
		u, err := url.Parse(endpoint)
		if err != nil {
			continue
		}
		query := u.Query()
		for _, app := range appFilter {
			query.Add("app", app)
		}
		for _, gpu := range gpuFilter {
			query.Add("gpu", gpu)
		}
		u.RawQuery = query.Encode()
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
		if err != nil {
			continue
		}
		response, err := s.discoveryClient.Do(request)
		if err != nil {
			continue
		}
		if response.StatusCode != 200 {
			_ = response.Body.Close()
			continue
		}
		var entries []struct {
			Address string             `json:"address"`
			Runners []discoveredRunner `json:"runners"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&entries)
		_ = response.Body.Close()
		if decodeErr != nil {
			continue
		}
		for _, entry := range entries {
			if _, err := destination.ValidateURL(entry.Address); err != nil {
				continue
			}
			filtered := make([]discoveredRunner, 0, len(entry.Runners))
			for _, runner := range entry.Runners {
				if runner.URL == "" || runner.App == "" {
					continue
				}
				if len(appFilter) > 0 {
					match := false
					for _, app := range appFilter {
						if runner.App == app {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
				if len(gpuFilter) > 0 {
					name := ""
					if runner.GPU != nil {
						name = strings.TrimSpace(runner.GPU.Name)
					}
					match := false
					for _, gpu := range gpuFilter {
						if name == gpu {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
				filtered = append(filtered, runner)
			}
			if len(filtered) > 0 {
				result = append(result, discoveredOrchestrator{Address: entry.Address, Score: 1, Runners: filtered, LastSeen: time.Now().UTC()})
			}
		}
	}
	if len(result) == 0 {
		signerError(w, http.StatusServiceUnavailable, "orchestrator discovery unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func randomStateID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type paymentFailure struct {
	status int
	reason string
}

func (e paymentFailure) Error() string { return e.reason }
func invalid(reason string) error      { return paymentFailure{http.StatusBadRequest, reason} }

func checkMaxPrice(req paymentRequest, price signercompat.PriceInfo) error {
	if req.MaxPrice == nil {
		return nil
	}
	ceiling, ok := new(big.Rat).SetString(req.MaxPrice.Price.String())
	if !ok || ceiling.Sign() <= 0 || req.MaxPrice.Currency != "wei" {
		return invalid("maxPrice requires a positive wei price")
	}
	unit := map[string]string{"live": "seconds", "fixed": "fixed"}[req.Type]
	if req.MaxPrice.Unit != unit {
		return invalid("maxPrice unit does not match payment type")
	}
	actual := new(big.Rat).SetFrac64(price.PricePerUnit, price.UnitsPerPrice)
	if actual.Cmp(ceiling) > 0 {
		return paymentFailure{481, "orchestrator price exceeds maxPrice"}
	}
	return nil
}

func (s *Service) generate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		signerError(w, 400, "payment request too large")
		return
	}
	var req paymentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		signerError(w, 400, "invalid payment request")
		return
	}
	if req.Type != "live" && req.Type != "fixed" {
		signerError(w, 400, "payment type must be live or fixed")
		return
	}
	if len(req.Capabilities) > 0 {
		signerError(w, 400, "payment capabilities are unsupported")
		return
	}
	info, err := signercompat.DecodeOrchestratorInfo(req.Orchestrator)
	if err != nil {
		signerError(w, 400, "invalid orchestrator Protobuf")
		return
	}
	if len(info.Address) != 20 || info.Price.PricePerUnit <= 0 || info.Price.UnitsPerPrice <= 0 || len(info.TicketParams.Recipient) != 20 || len(info.TicketParams.RecipientRandHash) != 32 || info.Auth.SessionID == "" {
		signerError(w, 400, "incomplete or expired orchestrator payment params")
		return
	}
	if info.Auth.Expiration <= time.Now().Unix() {
		w.Header().Set("Livepeer-Orchestrator-URL", info.Transcoder)
		signerError(w, 480, "refresh session for remote signer")
		return
	}
	if req.ManifestID == "" {
		req.ManifestID = info.Auth.SessionID
	}
	if req.ManifestID != info.Auth.SessionID {
		signerError(w, 400, "manifest ID does not match payment params")
		return
	}
	if err := checkMaxPrice(req, info.Price); err != nil {
		var f paymentFailure
		if errors.As(err, &f) {
			signerError(w, f.status, f.reason)
		} else {
			signerError(w, 400, err.Error())
		}
		return
	}
	address := ethcommon.BytesToAddress(info.Address)
	var state paymentState
	oldSequence := int64(-1)
	if len(req.State.State) > 0 || len(req.State.Sig) > 0 {
		if !(pm.DefaultSigVerifier{}).Verify(s.key.Address(), req.State.State, req.State.Sig) {
			signerError(w, 400, "invalid state signature")
			return
		}
		if err := json.Unmarshal(req.State.State, &state); err != nil || state.StateID == "" || state.SequenceNumber > math.MaxInt64 {
			signerError(w, 400, "invalid payment state")
			return
		}
		if state.OrchestratorAddress != address || state.App != req.App || state.Type != req.Type || state.ManifestID != req.ManifestID {
			signerError(w, 400, "payment state scope mismatch")
			return
		}
		oldSequence = int64(state.SequenceNumber)
	} else {
		id, err := randomStateID()
		if err != nil {
			signerError(w, 500, "cannot create payment state")
			return
		}
		state = paymentState{StateID: id, OrchestratorAddress: address, App: req.App, Type: req.Type, ManifestID: req.ManifestID, InitialPricePerUnit: info.Price.PricePerUnit, InitialPixelsPerUnit: info.Price.UnitsPerPrice}
	}

	if state.InitialPricePerUnit <= 0 || state.InitialPixelsPerUnit <= 0 || new(big.Rat).SetFrac64(info.Price.PricePerUnit, info.Price.UnitsPerPrice).Cmp(new(big.Rat).SetFrac64(state.InitialPricePerUnit, state.InitialPixelsPerUnit)) > 0 {
		signerError(w, 481, "orchestrator price exceeds initial session price")
		return
	}
	response, err := s.store.apply(state.StateID, oldSequence, body, func() ([]byte, error) { return s.makePayment(r.Context(), req, info, state, oldSequence) })
	if err != nil {
		var f paymentFailure
		switch {
		case errors.As(err, &f):
			if f.status == 480 {
				w.Header().Set("Livepeer-Orchestrator-URL", info.Transcoder)
			}
			signerError(w, f.status, f.reason)
		case errors.Is(err, errStateConflict):
			signerError(w, 409, err.Error())
		default:
			signerError(w, 500, "payment generation failed")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(response)
}

func (s *Service) makePayment(ctx context.Context, req paymentRequest, info signercompat.OrchestratorInfo, state paymentState, oldSequence int64) ([]byte, error) {
	now := time.Now().UTC()
	seconds := int64(1)
	if req.Type == "live" {
		if oldSequence < 0 {
			seconds = 10
		} else {
			seconds = int64(math.Ceil(now.Sub(state.LastUpdate).Seconds()))
			if seconds < 1 {
				seconds = 1
			}
		}
		if seconds > 3600 {
			return nil, invalid("payment interval exceeds one hour")
		}
	}
	fee := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(info.Price.PricePerUnit), big.NewInt(seconds)), big.NewInt(info.Price.UnitsPerPrice))
	balance := new(big.Rat)
	if state.Balance != "" {
		if _, ok := balance.SetString(state.Balance); !ok {
			return nil, invalid("invalid balance in payment state")
		}
	}
	params := pm.TicketParams{Recipient: ethcommon.BytesToAddress(info.TicketParams.Recipient), FaceValue: new(big.Int).SetBytes(info.TicketParams.FaceValue), WinProb: new(big.Int).SetBytes(info.TicketParams.WinProb), RecipientRandHash: ethcommon.BytesToHash(info.TicketParams.RecipientRandHash), Seed: new(big.Int).SetBytes(info.TicketParams.Seed), ExpirationBlock: new(big.Int).SetBytes(info.TicketParams.ExpirationBlock), ExpirationParams: &pm.TicketExpirationParams{CreationRound: info.TicketParams.Expiration.CreationRound, CreationRoundBlockHash: ethcommon.BytesToHash(info.TicketParams.Expiration.CreationRoundBlockHash)}}
	if s.paymentChain == nil {
		return nil, paymentFailure{482, "sender chain unavailable"}
	}
	funds, err := s.paymentChain.SenderInfo(ctx, s.key.Address(), params.Recipient)
	if err != nil {
		return nil, paymentFailure{482, "sender chain observation failed"}
	}
	if funds.Snapshot.Block == nil || funds.Snapshot.Round == nil || params.ExpirationBlock.Cmp(new(big.Int).Add(funds.Snapshot.Block, big.NewInt(1))) <= 0 || params.ExpirationParams.CreationRound < funds.Snapshot.Round.Int64()-2 || params.ExpirationParams.CreationRound > funds.Snapshot.Round.Int64() {
		return nil, paymentFailure{480, "refresh session for remote signer"}
	}
	count, err := pm.RemoteBatchSize(params, fee, balance)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if err := s.senderPolicy.Check(params, count, funds); err != nil {
		return nil, invalid(err.Error())
	}

	if state.PMSessionID != params.RecipientRandHash.Hex() {
		state.SenderNonce = 0
		state.PMSessionID = params.RecipientRandHash.Hex()
	}
	if state.SenderNonce >= 500 {
		return nil, paymentFailure{480, "refresh session for remote signer"}
	}
	batch, remaining, err := pm.MakeRemoteBatch(params, s.key, state.SenderNonce, fee, balance)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "no tickets"):
			return nil, paymentFailure{482, "no tickets"}
		case strings.Contains(err.Error(), "refresh"):
			return nil, paymentFailure{480, "refresh session for remote signer"}
		default:
			return nil, invalid(err.Error())
		}
	}
	senderParams := make([]signercompat.TicketSenderParams, 0, len(batch.SenderParams))
	for _, sp := range batch.SenderParams {
		senderParams = append(senderParams, signercompat.TicketSenderParams{SenderNonce: sp.SenderNonce, Sig: sp.Sig})
	}
	message := signercompat.Payment{TicketParams: info.TicketParams, Sender: s.key.Address().Bytes(), Expiration: info.TicketParams.Expiration, SenderParams: senderParams, ExpectedPrice: info.Price}
	segHash := crypto.Keccak256(nil)
	flatten := append([]byte(req.ManifestID), make([]byte, 32)...)
	flatten = append(flatten, segHash...)
	sig, err := s.key.SignMessage(flatten)
	if err != nil {
		return nil, err
	}
	segment := signercompat.SegData{ManifestID: []byte(req.ManifestID), Hash: segHash, Signature: sig, Auth: info.Auth}
	state.SenderNonce = batch.SenderParams[len(batch.SenderParams)-1].SenderNonce
	state.Balance = remaining.RatString()
	state.LastUpdate = now
	state.AuthExpiry = info.Auth.Expiration
	state.SequenceNumber = uint64(oldSequence + 1)
	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	stateSig, err := s.key.SignMessage(stateBytes)
	if err != nil {
		return nil, err
	}
	return json.Marshal(paymentResponse{Payment: base64.StdEncoding.EncodeToString(signercompat.EncodePayment(message)), SegCreds: base64.StdEncoding.EncodeToString(signercompat.EncodeSegData(segment)), State: signedState{State: stateBytes, Sig: stateSig}})
}
