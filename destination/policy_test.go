package destination

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublic(t *testing.T) {
	p, err := New("runner", nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // An enforcement regression must not open outbound connections.
	for _, raw := range []string{"127.0.0.1", "10.1.1.1", "169.254.169.254", "100.64.1.1", "192.168.1.1", "192.88.99.1", "::1", "fc00::1", "fe80::1", "2001:db8::1", "2002::1", "::ffff:127.0.0.1"} {
		require.False(t, Public(netip.MustParseAddr(raw)), raw)
		_, err := p.DialContext(ctx, "tcp", net.JoinHostPort(raw, "443"))
		require.ErrorContains(t, err, "denied address", raw)
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		require.True(t, Public(netip.MustParseAddr(raw)), raw)
	}
}

func TestRedirectChecksDestinationAndDropsAuthorization(t *testing.T) {
	headers := make(chan http.Header, 1)
	var connections atomic.Int32
	var requests atomic.Int32
	final := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	final.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	final.Start()
	defer final.Close()
	credentialURL := strings.Replace(final.URL, "http://", "http://user:password@", 1)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		target := credentialURL
		if r.URL.Path == "/loop" {
			target = r.URL.String()
		}
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer redirect.Close()
	policy, err := New("runner", []string{redirect.URL})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodGet, redirect.URL, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "private-token")
	request.Header.Set("Proxy-Authorization", "private-proxy-token")
	client := policy.Client()
	client.Timeout = time.Second
	defer client.CloseIdleConnections()
	_, err = client.Do(request)
	require.ErrorContains(t, err, "denied address")
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, final.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	for _, address := range []string{"http://10.1.2.3", "https://10.1.2.3"} {
		_, err = client.Get(address)
		require.ErrorContains(t, err, "denied address")
	}
	require.Zero(t, connections.Load(), "redirect and proxy targets must remain unconnected")
	policy, err = New("runner", []string{redirect.URL, final.URL})
	require.NoError(t, err)
	client = policy.Client()
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	received := <-headers
	require.Empty(t, received.Get("Authorization"))
	require.Empty(t, received.Get("Proxy-Authorization"))
	require.Nil(t, response.Request.URL.User)
	requests.Store(0)
	response, err = client.Get(redirect.URL + "/loop")
	require.ErrorContains(t, err, "too many redirects")
	if response != nil {
		_ = response.Body.Close()
	}
	require.Equal(t, int32(10), requests.Load())
}

func TestCustomCAIsScopedAndVerified(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	policy, err := New("runner", []string{parsed.Host})
	require.NoError(t, err)
	_, err = policy.Client().Get(server.URL)
	require.Error(t, err, "untrusted certificate must fail")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "runner-ca.pem")
	require.NoError(t, os.WriteFile(path, certPEM, 0600))
	trusted, err := policy.WithCAFile(path)
	require.NoError(t, err)
	response, err := trusted.Client().Get(server.URL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	_, err = policy.WithCAFile(filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "cannot be read")
	require.NotContains(t, err.Error(), "missing")
}

func TestGrants(t *testing.T) {
	for raw, address := range map[string]string{
		"orch.internal": "orch.internal:443", "localhost": "localhost:443", "//orch.internal": "orch.internal:443",
		"https://orch.internal": "orch.internal:443", "http://orch.internal": "orch.internal:80",
		"127.0.0.1:8935": "127.0.0.1:8935", "http://orch.internal:443": "orch.internal:443",
		"https://orch.internal:00443": "orch.internal:00443",
		"[fe80::1%en0]:8935":          "[fe80::1%en0]:8935", "https://[fe80::1%25en0]:8935": "[fe80::1%en0]:8935",
		"[::1]": "[::1]:443", "http://[::1]": "[::1]:80", "https://[::1]:8935": "[::1]:8935",
	} {
		p, err := New("discovery", []string{raw})
		require.NoError(t, err, raw)
		require.Contains(t, p.Grants, address)
	}
	for _, raw := range []string{"", "ftp://orch", ":443", "orch:", "orch:0", "orch:65536", "orch:abc", "::1", "[bad]", "one,two", "*.internal", "https://user:password@orch", "orch/", "orch/path", "orch?", "orch?x=y", "orch#fragment"} {
		_, err := New("discovery", []string{raw})
		require.Error(t, err, raw)
		require.NotContains(t, err.Error(), "password")
	}
}

func TestDNSAnswersRespectGrants(t *testing.T) {
	p, err := New("runner", nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, ips := range [][]net.IPAddr{
		{{IP: net.ParseIP("127.0.0.1")}}, {{IP: net.ParseIP("fc00::1")}},
		{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("10.1.1.1")}},
	} {
		p.Lookup = func(context.Context, string) ([]net.IPAddr, error) { return ips, nil }
		_, err := p.DialContext(ctx, "tcp", "orch.example:443")
		require.ErrorContains(t, err, "denied address")
	}
	granted, err := New("runner", []string{"orch.example:443"})
	require.NoError(t, err)
	granted.Lookup = p.Lookup
	_, err = granted.DialContext(ctx, "tcp", "orch.example:443")
	require.ErrorIs(t, err, context.Canceled, "the exact hostname grant permits these DNS answers")
}

func TestDialContextRechecksDNSOnEachConnection(t *testing.T) {
	p, err := New("signer-discovery", nil)
	require.NoError(t, err)
	lookups := 0
	p.Lookup = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		if lookups == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Permit the public address through policy without connecting to it.
	_, err = p.DialContext(ctx, "tcp", "orch.example:443")
	require.ErrorIs(t, err, context.Canceled)
	_, err = p.DialContext(ctx, "tcp", "orch.example:443")
	require.ErrorContains(t, err, "denied address")
	require.Equal(t, 2, lookups)
}

func TestValidateRequiredURL(t *testing.T) {
	for _, endpoint := range []*url.URL{nil, {}, {Scheme: "https"}, {Host: "example.com"}, {Scheme: "ftp", Host: "example.com"}} {
		require.Error(t, ValidateURL(endpoint))
	}
	require.NoError(t, ValidateURL(&url.URL{Scheme: "https", Host: "example.com"}))
	_, err := ParseURL("")
	require.Error(t, err)
}
