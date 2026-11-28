// Package publichttp restricts untrusted HTTP requests to public IP addresses.
package publichttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

var ErrUnsafeAddress = errors.New("HTTP address must be public")

var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// Retain the standard defaults before applications install instrumentation or
// test RoundTrippers. Its dial hooks are never taken from a later custom client.
var standardTransport = http.DefaultTransport.(*http.Transport).Clone()

// IsPublicIP also rejects IPv4-mapped private addresses and shared address space.
func IsPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !sharedAddressSpace.Contains(ip)
}

// CheckPublicAddress runs after DNS resolution, immediately before connecting.
func CheckPublicAddress(_ string, address string, _ syscall.RawConn) error {
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil || !IsPublicIP(endpoint.Addr()) {
		return ErrUnsafeAddress
	}
	return nil
}

// NewTransport protects every connection, including redirects and DNS changes.
// Environment proxies cannot perform a second, unchecked resolution for us.
func NewTransport() *http.Transport {
	transport := standardTransport.Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: CheckPublicAddress}
	transport.DialContext = dialer.DialContext
	return transport
}

type defaultTransport struct {
	public *http.Transport
}

// NewDefaultTransport also honors an application's explicitly installed custom
// RoundTripper. Such adapters own their transport behavior; the standard HTTP
// transport always uses the protected dialer, regardless of when it is used.
func NewDefaultTransport() http.RoundTripper {
	return &defaultTransport{public: NewTransport()}
}

func (transport *defaultTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := ValidateTarget(request.URL); err != nil {
		return nil, err
	}
	if current := http.DefaultTransport; current != nil && current != transport {
		if _, standard := current.(*http.Transport); !standard {
			return current.RoundTrip(request)
		}
	}
	return transport.public.RoundTrip(request)
}

func (transport *defaultTransport) CloseIdleConnections() {
	transport.public.CloseIdleConnections()
}

// ValidateTarget rejects unsafe literal targets without resolving domain names.
// The transport checks domains at connection time, keeping redirect validation
// usable with mock RoundTrippers and avoiding a redundant DNS lookup.
func ValidateTarget(value *url.URL) error {
	if value == nil || (value.Scheme != "http" && value.Scheme != "https") || value.Hostname() == "" {
		return ErrUnsafeAddress
	}
	host := strings.TrimSuffix(strings.ToLower(value.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return ErrUnsafeAddress
	}
	if ip, err := netip.ParseAddr(host); err == nil && !IsPublicIP(ip) {
		return ErrUnsafeAddress
	}
	return nil
}

// ValidateURL also checks DNS before dispatch. NewTransport remains necessary
// because DNS can change between this validation and opening the connection.
func ValidateURL(ctx context.Context, value *url.URL) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateTarget(value); err != nil {
		return err
	}
	host := strings.TrimSuffix(strings.ToLower(value.Hostname()), ".")
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	if len(addresses) == 0 {
		return ErrUnsafeAddress
	}
	for _, address := range addresses {
		ip, ok := netip.AddrFromSlice(address.IP)
		if !ok || !IsPublicIP(ip) {
			return ErrUnsafeAddress
		}
	}
	return nil
}
