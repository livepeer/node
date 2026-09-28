package destination

import (
	"context"
	"net"
	"net/netip"
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
