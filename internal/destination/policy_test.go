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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublic(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.1.1", "169.254.169.254", "100.64.1.1", "192.168.1.1", "192.88.99.1", "::1", "fc00::1", "fe80::1", "2001:db8::1", "2002::1"} {
		require.False(t, Public(netip.MustParseAddr(raw)), raw)
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		require.True(t, Public(netip.MustParseAddr(raw)), raw)
	}
}

func TestRedirectChecksDestinationAndDropsAuthorization(t *testing.T) {
	var receivedAuthorization string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer final.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirect.Close()
	first, err := url.Parse(redirect.URL)
	require.NoError(t, err)
	second, err := url.Parse(final.URL)
	require.NoError(t, err)
	policy, err := New("runner", []string{first.Host})
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodGet, redirect.URL, nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "private-token")
	_, err = policy.Client().Do(request)
	require.ErrorContains(t, err, "denied address")
	policy, err = New("runner", []string{first.Host, second.Host})
	require.NoError(t, err)
	response, err := policy.Client().Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Empty(t, receivedAuthorization)
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

func TestGrantRequiresExactValidPort(t *testing.T) {
	for _, raw := range []string{"localhost", "localhost:0", "localhost:65536", "localhost:abc", ":443", "localhost:443/path"} {
		_, err := New("runner", []string{raw})
		require.Error(t, err, raw)
	}
	_, err := New("runner", []string{"127.0.0.1:443"})
	require.NoError(t, err)
}

func TestMixedDNSDeniedBeforeDial(t *testing.T) {
	p, err := New("runner", nil)
	require.NoError(t, err)
	p.Lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	_, err = p.DialContext(context.Background(), "tcp", "runner.example:443")
	require.ErrorContains(t, err, "denied address")
	require.True(t, strings.Contains(err.Error(), "runner"))
}
