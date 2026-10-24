package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"perplexity-bridge/internal/netutil"
)

const (
	chromeH2ReadIdleTimeout  = 10 * time.Second
	chromeH2PingTimeout      = 5 * time.Second
	chromeH2WriteByteTimeout = 10 * time.Second
)

var chromeH2IdleConnTimeout = 90 * time.Second

type chromeDynamicRoundTripper struct{}

func (chromeDynamicRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return currentChromeH2Transport().RoundTrip(req)
}

func (chromeDynamicRoundTripper) CloseIdleConnections() {
	currentChromeH2Transport().CloseIdleConnections()
}

var (
	chromeRoundTripperMu sync.RWMutex
	chromeRoundTripperH2 *http2.Transport
	chromeRoundTripper   http.RoundTripper = chromeDynamicRoundTripper{}
)

func newChromeH2Transport() *http2.Transport {
	return &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return dialChromeTLS(ctx, network, addr)
		},
		DisableCompression: true,
		IdleConnTimeout:    chromeH2IdleConnTimeout,
		ReadIdleTimeout:    chromeH2ReadIdleTimeout,
		PingTimeout:        chromeH2PingTimeout,
		WriteByteTimeout:   chromeH2WriteByteTimeout,
	}
}

func currentChromeH2Transport() *http2.Transport {
	chromeRoundTripperMu.RLock()
	h2 := chromeRoundTripperH2
	chromeRoundTripperMu.RUnlock()
	if h2 != nil {
		return h2
	}
	chromeRoundTripperMu.Lock()
	if chromeRoundTripperH2 == nil {
		chromeRoundTripperH2 = newChromeH2Transport()
	}
	h2 = chromeRoundTripperH2
	chromeRoundTripperMu.Unlock()
	return h2
}

func getChromeRoundTripper() http.RoundTripper {
	currentChromeH2Transport()
	return chromeRoundTripper
}

func RebuildChromeTransport() {
	chromeRoundTripperMu.Lock()
	old := chromeRoundTripperH2
	chromeRoundTripperH2 = newChromeH2Transport()
	chromeRoundTripperMu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

func dialChromeTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	dialCtx := ctx
	if to := AppConfig.TLSDialTimeoutDuration(); to > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}

	rawConn, err := netutil.DialThroughProxy(dialCtx, network, addr, AppConfig.UpstreamProxyURL())
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}

	tlsConfig := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	tlsConn := utls.UClient(rawConn, tlsConfig, utls.HelloChrome_Auto)

	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}

	return tlsConn, nil
}

func getChromeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: getChromeRoundTripper(),
		Timeout:   timeout,
	}
}
