package publichttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (transport testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestDefaultTransportHonorsLateAdaptersAndGuardsTargets(t *testing.T) {
	transport := NewDefaultTransport()
	client := &http.Client{Transport: transport}
	defer client.CloseIdleConnections()
	previous := http.DefaultTransport
	defer func() { http.DefaultTransport = previous }()
	requests := 0
	http.DefaultTransport = testRoundTripper(func(request *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("mock")), Request: request}, nil
	})
	response, err := client.Get("https://public.example/path")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	for _, raw := range []string{"http://localhost/private", "http://127.0.0.1/private", "http://100.100.100.200/private"} {
		if _, err := client.Get(raw); !errors.Is(err, ErrUnsafeAddress) {
			t.Errorf("unsafe literal reached adapter: %s, %v", raw, err)
		}
	}
	if requests != 1 {
		t.Fatalf("requests=%d, want one mocked public request", requests)
	}
}

func TestPublicAddressGuard(t *testing.T) {
	for _, address := range []string{"8.8.8.8:443", "[2606:4700:4700::1111]:80"} {
		if err := CheckPublicAddress("tcp", address, nil); err != nil {
			t.Errorf("public address %s rejected: %v", address, err)
		}
	}
	for _, address := range []string{
		"127.0.0.1:80", "10.1.2.3:80", "172.16.1.2:80", "192.168.1.1:80",
		"169.254.169.254:80", "100.64.0.1:80", "100.100.100.200:80", "100.127.255.255:80",
		"0.0.0.0:80", "224.0.0.1:80", "[::1]:80", "[::]:80", "[fe80::1]:80",
		"[fd00::1]:80", "[ff02::1]:80", "[::ffff:100.100.100.200]:80", "invalid",
	} {
		if !errors.Is(CheckPublicAddress("tcp", address, nil), ErrUnsafeAddress) {
			t.Errorf("non-public address %s accepted", address)
		}
	}
}

func TestTransportRejectsDNSRebindingAtConnection(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	dns, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer dns.Close()
	var lookups atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 4096)
		for {
			n, address, err := dns.ReadFrom(buffer)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(buffer[:n]) != nil || len(query.Questions) != 1 {
				continue
			}
			question := query.Questions[0]
			answer := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions}
			if question.Type == dnsmessage.TypeA {
				ip := [4]byte{127, 0, 0, 1}
				if lookups.Add(1) == 1 {
					ip = [4]byte{8, 8, 8, 8}
				}
				answer.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, Type: dnsmessage.TypeA}, Body: &dnsmessage.AResource{A: ip}}}
			}
			data, err := answer.Pack()
			if err == nil {
				_, _ = dns.WriteTo(data, address)
			}
		}
	}()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", dns.LocalAddr().String())
	}}
	defer func() { net.DefaultResolver = previous; dns.Close(); <-done }()
	local, _ := url.Parse(server.URL)
	target, _ := url.Parse("http://rebind.example:" + local.Port() + "/cat.jpg")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ValidateURL(ctx, target); err != nil {
		t.Fatalf("initial public DNS result rejected: %v", err)
	}
	transport := NewTransport()
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil {
		t.Fatal("proxy could bypass connected IP checks")
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if response != nil {
		response.Body.Close()
	}
	if !errors.Is(err, ErrUnsafeAddress) || hits.Load() != 0 || lookups.Load() < 2 {
		t.Fatalf("DNS rebinding was not blocked: err=%v requests=%d DNS lookups=%d", err, hits.Load(), lookups.Load())
	}
}
