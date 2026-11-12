package redeemer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/j0sh/boa/pkg/boa"
	"github.com/livepeer/node/eth"
	"github.com/livepeer/node/internal/test"
	"github.com/livepeer/node/nodeconfig"
	"github.com/livepeer/node/pm"
	"github.com/livepeer/node/redemption"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func bindAddress(t *testing.T) boa.HTTPBind {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	var bind boa.HTTPBind
	require.NoError(t, bind.UnmarshalText([]byte(address)))
	return bind
}

func serviceParams(t *testing.T, f *test.RedemptionRPC) Params {
	t.Helper()
	file, password := test.WriteFixedKeystore(t)
	key, err := eth.OpenKeystoreFile(file, password)
	require.NoError(t, err)
	f.Mu.Lock()
	f.Wallet = key.Address()
	f.Mu.Unlock()
	rpc, err := url.Parse(f.Server.URL)
	require.NoError(t, err)
	id := uint64(1)
	controller := common.HexToAddress("0x1000")
	return Params{Settings: nodeconfig.Settings{Network: "test", DataDir: t.TempDir()}, Listen: bindAddress(t), MetricsListen: bindAddress(t), RedeemerDB: filepath.Join(t.TempDir(), "tickets.sqlite"), KeystoreFile: file, KeystorePasswordFile: password, RPCURL: rpc, ChainID: &id, Controller: &controller, RedeemerTxTimeout: time.Second, RedeemerMaxTxReplacements: 1, MaxPendingTickets: 32, MaxTicketsPerRecipient: 8}
}

func TestDedicatedWalletStartupPolicy(t *testing.T) {
	for _, kind := range []string{"registered", "active", "unavailable", "wrong-chain"} {
		t.Run(kind, func(t *testing.T) {
			f := test.NewRedemptionRPC(t)
			p := serviceParams(t, f)
			f.Mu.Lock()
			switch kind {
			case "registered":
				f.Registered = true
			case "active":
				f.WalletActive = true
			case "unavailable":
				f.SnapshotError = true
			case "wrong-chain":
				*p.ChainID = 2
			}
			f.Mu.Unlock()
			require.Error(t, Serve(t.Context(), p, io.Discard))
			require.NoFileExists(t, p.RedeemerDB, "wallet and chain checks precede database creation")
		})
	}
}

func TestServeProbesAndShutdown(t *testing.T) {
	for _, tc := range []struct{ busy, auth bool }{{}, {busy: true}, {auth: true}} {
		t.Run(fmt.Sprintf("busy=%t/auth=%t", tc.busy, tc.auth), func(t *testing.T) {
			f := test.NewRedemptionRPC(t)
			p := serviceParams(t, f)
			var calls atomic.Int32
			if tc.auth {
				webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.WriteString(w, `{"status":403}`)
				}))
				defer webhook.Close()
				p.AuthWebhook, _ = url.Parse(webhook.URL)
			}
			if tc.busy {
				seedBusyQueue(t, p)
				handler := f.Server.Config.Handler
				selector := fmt.Sprintf("0x%x", crypto.Keccak256([]byte("usedTickets(bytes32)"))[:4])
				f.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					var request struct {
						Method string
						Params []json.RawMessage
					}
					require.NoError(t, json.Unmarshal(body, &request))
					if request.Method == "eth_call" {
						var call struct{ Input string }
						require.NoError(t, json.Unmarshal(request.Params[0], &call))
						if strings.HasPrefix(call.Input, selector) {
							<-r.Context().Done()
							return
						}
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					handler.ServeHTTP(w, r)
				})
			}
			wait := 5 * time.Second
			if tc.busy {
				wait = 40 * time.Second
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- Serve(ctx, p, io.Discard) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(3 * time.Second):
					t.Error("redeemer did not stop")
				}
			})
			client := &http.Client{Timeout: time.Second}
			defer client.CloseIdleConnections()
			ready := func() bool {
				response, err := client.Get("http://" + p.MetricsListen.String() + "/readyz")
				if err != nil {
					return false
				}
				defer response.Body.Close()
				return response.StatusCode == 200
			}
			require.Eventually(t, ready, wait, 10*time.Millisecond)
			response, err := client.Get("http://" + p.MetricsListen.String() + "/metrics")
			require.NoError(t, err)
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Contains(t, string(data), `component="redeemer"`)
			response, err = client.Get("http://" + p.Listen.String() + "/tickets")
			require.NoError(t, err)
			response.Body.Close()
			require.Equal(t, 405, response.StatusCode)
			if tc.auth {
				require.Zero(t, calls.Load(), "probes and other methods do not authorize")
				response, err = client.Post("http://"+p.Listen.String()+"/tickets", "application/json", strings.NewReader("invalid ticket"))
				require.NoError(t, err)
				response.Body.Close()
				require.Equal(t, 403, response.StatusCode, "invalid submissions are still authorized")
				require.True(t, ready(), "authorization denial does not change readiness")
				require.EqualValues(t, 1, calls.Load())
			}
			info, err := os.Stat(p.RedeemerDB)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		})
	}
}

func seedBusyQueue(t *testing.T, p Params) {
	t.Helper()
	store, err := redemption.OpenRedeemerDB(p.RedeemerDB)
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, store.Bind(t.Context(), redemption.Binding{ChainID: "1", Controller: *p.Controller, Role: "redeemer", Destination: "chain"}, redemption.DefaultLimits, nil))
	payer, err := eth.OpenKeystoreFile(test.WriteFixedKeystore(t))
	require.NoError(t, err)
	for nonce := uint32(1); nonce <= 3; nonce++ {
		random := new(big.Int).SetUint64(uint64(nonce))
		ticket := &pm.SignedTicket{Ticket: &pm.Ticket{PayerAddress: payer.Address(), Recipient: common.HexToAddress("0x5678"), FaceValue: big.NewInt(1e12), WinProb: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), TicketNonce: nonce, RecipientRandHash: crypto.Keccak256Hash(common.LeftPadBytes(random.Bytes(), 32)), CreationRound: 1, CreationRoundBlockHash: common.HexToHash("0x1"), ParamsExpirationBlock: big.NewInt(10)}, RecipientRand: random}
		ticket.Sig, err = payer.SignMessage(ticket.Hash().Bytes())
		require.NoError(t, err)
		require.NoError(t, store.Admit(t.Context(), ticket))
	}
}

func TestPrintConfigRedactsCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LIVEPEER_REDEEMER_RPC_URL", "http://private.example/secret-token")
	t.Setenv("LIVEPEER_REDEEMER_KEYSTORE_PASSWORD", "wallet-secret")
	t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", "https://example.com/?auth-url-secret")
	t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK_HEADERS", "Authorization: Bearer auth-header-secret")
	var out, errOut bytes.Buffer
	cmd := Root(&out, &errOut)
	cmd.SetArgs([]string{"--config=", "--print-config"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), "8941")
	require.NotContains(t, out.String(), "secret-token")
	require.NotContains(t, out.String(), "wallet-secret")
	require.NotContains(t, out.String(), "auth-url-secret")
	require.NotContains(t, out.String(), "auth-header-secret")
}

func loadAuthConfig(t *testing.T, p Params, args ...string) (Params, error) {
	t.Helper()
	cmd := nodeconfig.Command("redeemer", nodeconfig.Mainnet, boa.Cmd[Params]{Params: &p, RejectUnknown: true,
		RunFuncCtxE: func(ctx *boa.HookContext, p *Params, _ *cobra.Command, _ []string) error {
			dump, err := ctx.DumpBytes(".toml", nil)
			require.NoError(t, err)
			require.NotContains(t, string(dump), "private-detail")
			return p.Validate()
		},
	}, &p.ConfigFile)
	err := cmd.RunArgsE(args)
	return p, err
}

func TestAuthWebhookConfiguration(t *testing.T) {
	base := serviceParams(t, test.NewRedemptionRPC(t))
	const endpoint = "https://webhook.example/auth?token=private-detail"
	const headers = `Authorization: Bearer private-detail, X-Tag: one, x-tag: two, "X-List: a,b"`
	for _, tc := range []struct {
		name, field, content string
		withURL              bool
	}{
		{"empty URL file", "AUTH_WEBHOOK_FILE", "", false},
		{"empty orphan headers file", "AUTH_WEBHOOK_HEADERS_FILE", "", false},
		{"blank orphan headers file", "AUTH_WEBHOOK_HEADERS_FILE", " \t\n", false},
		{"empty headers with URL", "AUTH_WEBHOOK_HEADERS_FILE", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "secret")
			require.NoError(t, os.WriteFile(file, []byte(tc.content), 0600))
			t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", "")
			t.Setenv("LIVEPEER_REDEEMER_"+tc.field, file)
			if tc.withURL {
				t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", endpoint)
			}
			_, err := loadAuthConfig(t, base, "--config=")
			if tc.withURL {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "auth webhook must be an absolute HTTP or HTTPS URL")
				require.NotContains(t, err.Error(), "private-detail")
			}
		})
	}
	for _, source := range []string{"environment", "TOML", "JSON", "file flags"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			uFile, hFile := filepath.Join(dir, "url"), filepath.Join(dir, "headers")
			require.NoError(t, os.WriteFile(uFile, []byte(endpoint), 0600))
			require.NoError(t, os.WriteFile(hFile, []byte(headers), 0600))
			args := []string{"--config="}
			switch source {
			case "environment":
				t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", endpoint)
				t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK_HEADERS", headers)
			case "file flags":
				args = append(args, "--auth-webhook-file", uFile, "--auth-webhook-headers-file", hFile)
			default:
				ext := ".toml"
				if source == "JSON" {
					ext = ".json"
				}
				path := filepath.Join(dir, "config"+ext)
				test.WriteConfig(t, path, "AuthWebhookFile = 'url'\nAuthWebhookHeadersFile = 'headers'\n")
				args = []string{"--config", path}
			}
			p, err := loadAuthConfig(t, base, args...)
			require.NoError(t, err)
			require.Equal(t, endpoint, p.AuthWebhook.String())
			require.Equal(t, "Bearer private-detail", http.Header(p.AuthWebhookHeaders).Get("Authorization"))
			require.Equal(t, []string{"one", "two"}, http.Header(p.AuthWebhookHeaders).Values("X-Tag"))
			require.Equal(t, "a,b", http.Header(p.AuthWebhookHeaders).Get("X-List"))
		})
	}
	for _, flag := range []string{"--auth-webhook", "--auth-webhook-headers"} {
		_, err := loadAuthConfig(t, base, "--config=", flag, "private-detail")
		require.ErrorContains(t, err, "unknown flag")
		require.NotContains(t, err.Error(), "private-detail")
	}
	for _, field := range []string{"AUTH_WEBHOOK", "AUTH_WEBHOOK_HEADERS"} {
		t.Run(field+" file protection", func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "secret")
			value := endpoint
			if field == "AUTH_WEBHOOK_HEADERS" {
				value = headers
			}
			require.NoError(t, os.WriteFile(file, []byte(value), 0600))
			t.Setenv("LIVEPEER_REDEEMER_AUTH_WEBHOOK", endpoint)
			t.Setenv("LIVEPEER_REDEEMER_"+field+"_FILE", file)
			if field == "AUTH_WEBHOOK_HEADERS" {
				t.Setenv("LIVEPEER_REDEEMER_"+field, headers)
			}
			_, err := loadAuthConfig(t, base, "--config=")
			require.ErrorContains(t, err, "cannot both be set")
			require.NotContains(t, err.Error(), "private-detail")
			t.Setenv("LIVEPEER_REDEEMER_"+field, "")
			require.NoError(t, os.Chmod(file, 0644))
			_, err = loadAuthConfig(t, base, "--config=")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private-detail")
		})
	}
}

func TestAuthWebhookConfigurationValidation(t *testing.T) {
	p := serviceParams(t, test.NewRedemptionRPC(t))
	require.NoError(t, p.Validate(), "omitted webhook is disabled")
	for _, endpoint := range []string{"/private-detail", "ftp://private-detail", "https://user:private-detail@example.com", "https://example.com/#private-detail"} {
		p.AuthWebhook, _ = url.Parse(endpoint)
		err := p.Validate()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "private-detail")
	}
	p.AuthWebhook = nil
	p.AuthWebhookHeaders = nodeconfig.Headers{"Authorization": {"private-detail"}}
	require.Error(t, p.Validate(), "headers require an endpoint")
	p.AuthWebhook, _ = url.Parse("http://127.0.0.1/private?token=private-detail")
	require.NoError(t, p.Validate())
	p.AuthWebhookHeaders = nodeconfig.Headers{"Bad Name": {"private-detail"}}
	err := p.Validate()
	require.ErrorContains(t, err, "invalid auth webhook headers")
	require.NotContains(t, err.Error(), "private-detail")

}
