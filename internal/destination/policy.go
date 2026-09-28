// Package destination enforces outbound destination grants at dial time.
package destination

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var deniedV4 = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)
var deniedV6 = mustPrefixes("::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8", "2001::/23", "2001:db8::/32", "2002::/16")
var publicV6 = netip.MustParsePrefix("2000::/3")

func mustPrefixes(raw ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(raw))
	for _, item := range raw {
		result = append(result, netip.MustParsePrefix(item))
	}
	return result
}

func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return false
	}
	if ip.Is4() {
		for _, prefix := range deniedV4 {
			if prefix.Contains(ip) {
				return false
			}
		}
		return true
	}
	if !publicV6.Contains(ip) {
		return false
	}
	for _, prefix := range deniedV6 {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// Policy grants a purpose-specific list of exact host:port destinations.
// Keep separate instances for runners, webhooks, discovery and Ethereum RPC.
type Policy struct {
	Purpose string
	Grants  map[string]struct{}
	Lookup  func(context.Context, string) ([]net.IPAddr, error)
}

func New(purpose string, grants []string) (Policy, error) {
	p := Policy{Purpose: purpose, Grants: map[string]struct{}{}}
	for _, raw := range grants {
		host, port, err := net.SplitHostPort(raw)
		portNumber, portErr := strconv.Atoi(port)
		if err != nil || host == "" || portErr != nil || portNumber < 1 || portNumber > 65535 || strings.ContainsAny(host, "/@") {
			return Policy{}, fmt.Errorf("invalid %s destination grant: expected exact host:port", purpose)
		}
		p.Grants[strings.ToLower(net.JoinHostPort(host, port))] = struct{}{}
	}
	return p, nil
}

func (p Policy) granted(addr string) bool {
	_, ok := p.Grants[strings.ToLower(addr)]
	return ok
}

// ValidateURL checks URL syntax only. Address policy is intentionally applied
// again at every connection after DNS resolution, including redirects.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("destination must be an absolute HTTP or HTTPS URL")
	}
	return u, nil
}

func (p Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("destination has invalid host or port")
	}
	lookup := p.Lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupIPAddr
	}
	var ips []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{literal.Unmap()}
	} else {
		resolved, err := lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("%s DNS lookup failed: %w", p.Purpose, err)
		}
		for _, item := range resolved {
			ip, ok := netip.AddrFromSlice(item.IP)
			if ok {
				ips = append(ips, ip.Unmap())
			}
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s destination has no addresses", p.Purpose)
	}
	if !p.granted(net.JoinHostPort(host, port)) {
		for _, ip := range ips {
			if !Public(ip) {
				return nil, fmt.Errorf("%s destination contains a denied address; add an exact host:port grant for this purpose", p.Purpose)
			}
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var last error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, fmt.Errorf("%s connection failed: %w", p.Purpose, last)
}

func (p Policy) Transport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           p.DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		ResponseHeaderTimeout: headerTimeout,
		IdleConnTimeout:       90 * time.Second,
	}
}

func (p Policy) Client() *http.Client {
	return &http.Client{
		Transport: p.Transport(15 * time.Second),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			req.Header.Del("Authorization")
			req.Header.Del("Proxy-Authorization")
			if req.URL != nil {
				req.URL.User = nil
			}
			return nil
		},
	}
}
