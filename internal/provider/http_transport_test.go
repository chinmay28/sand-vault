package provider

import (
	"net/http"
	"testing"
)

func TestConfigureTransportSizesTheIdlePool(t *testing.T) {
	t.Cleanup(func() { ConfigureTransport(DefaultConnsPerHost) })

	if got := ConnsPerHost(); got != DefaultConnsPerHost {
		t.Fatalf("starts at %d idle connections per host, want %d", got, DefaultConnsPerHost)
	}

	ConfigureTransport(32)
	if got := ConnsPerHost(); got != 32 {
		t.Errorf("after ConfigureTransport(32): %d", got)
	}
	tr := httpClient.Transport.(*http.Transport)
	if tr.MaxIdleConns < 32 {
		t.Errorf("the pool across hosts (%d) is smaller than the per-host figure", tr.MaxIdleConns)
	}
	// Everything else is the default transport's: proxies, timeouts, HTTP/2.
	if tr.Proxy == nil || tr.TLSHandshakeTimeout == 0 {
		t.Error("the transport lost the defaults it was cloned from")
	}

	// Below two is clamped to two: a pool of one would serialise every cloud.
	ConfigureTransport(0)
	if got := ConnsPerHost(); got != 2 {
		t.Errorf("ConfigureTransport(0) gave %d, want 2", got)
	}

	// Setting what is already set swaps nothing.
	before := httpClient.Transport
	ConfigureTransport(2)
	if httpClient.Transport != before {
		t.Error("a no-op ConfigureTransport replaced the transport")
	}
}
