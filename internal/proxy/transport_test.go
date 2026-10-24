package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// Real h2c sockets exercise http2.Transport's idle accounting; a fake RoundTripper would not.
const testH2IdleTimeout = 150 * time.Millisecond

type trackedConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type h2cPool struct {
	mu    sync.Mutex
	conns []*trackedConn
}

func (p *h2cPool) dials() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.conns)
}

func (p *h2cPool) conn(t *testing.T, i int) *trackedConn {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.conns) {
		t.Fatalf("pool has %d connections, want index %d", len(p.conns), i)
	}
	return p.conns[i]
}

type h2cServer struct {
	url     string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *h2cServer) releaseAll() { s.once.Do(func() { close(s.release) }) }

func newH2cServer(t *testing.T) *h2cServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &h2cServer{url: "http://" + ln.Addr().String(), entered: make(chan struct{}, 16), release: make(chan struct{})}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			s.entered <- struct{}{}
			select {
			case <-s.release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "ok")
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go (&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: h})
		}
	}()
	t.Cleanup(func() { s.releaseAll(); _ = ln.Close() })
	return s
}

// configureH2c rewires the active transport to dial h2c. It deliberately leaves
// IdleConnTimeout as built by newChromeH2Transport so the production setting is
// what the tests exercise. Run it before the transport serves its first request.
func configureH2c(tr *http2.Transport) *h2cPool {
	pool := &h2cPool{}
	tr.AllowHTTP = true
	tr.DialTLSContext = func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		tc := &trackedConn{Conn: c, closed: make(chan struct{})}
		pool.mu.Lock()
		pool.conns = append(pool.conns, tc)
		pool.mu.Unlock()
		return tc, nil
	}
	return pool
}

// freshH2cTransport installs a new active transport through the production
// rebuild path (retiring the previous one) and rewires it for h2c.
func freshH2cTransport(t *testing.T) *h2cPool {
	t.Helper()
	prev := chromeH2IdleConnTimeout
	chromeH2IdleConnTimeout = testH2IdleTimeout
	t.Cleanup(func() {
		chromeH2IdleConnTimeout = prev
		RebuildChromeTransport()
	})
	RebuildChromeTransport()
	return configureH2c(currentChromeH2Transport())
}

func get(url string) error {
	resp, err := (&http.Client{Transport: getChromeRoundTripper(), Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func startHold(s *h2cServer) chan error {
	done := make(chan error, 1)
	go func() { done <- get(s.url + "/hold") }()
	return done
}

func waitEntered(t *testing.T, s *h2cServer) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached server")
	}
}

func waitClosed(t *testing.T, c *trackedConn, within time.Duration, what string) {
	t.Helper()
	select {
	case <-c.closed:
	case <-time.After(within):
		t.Fatalf("%s: connection still open after %v", what, within)
	}
}

func assertOpen(t *testing.T, c *trackedConn, what string) {
	t.Helper()
	select {
	case <-c.closed:
		t.Fatalf("%s: connection was closed", what)
	default:
	}
}

func waitDone(t *testing.T, done chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: request did not finish", what)
	}
}

func TestChromeH2TransportBoundsIdleConnections(t *testing.T) {
	tr := newChromeH2Transport()
	if tr.IdleConnTimeout <= 0 {
		t.Fatal("IdleConnTimeout must be set so retired pools reclaim connections")
	}
	// Idle reclaim is distinct from the ReadIdleTimeout health ping.
	if tr.IdleConnTimeout <= tr.ReadIdleTimeout+tr.PingTimeout {
		t.Fatalf("IdleConnTimeout=%v must exceed ping window %v", tr.IdleConnTimeout, tr.ReadIdleTimeout+tr.PingTimeout)
	}
}

func TestTransportFixtureRestoresIdlePolicy(t *testing.T) {
	before := chromeH2IdleConnTimeout
	t.Run("fixture", func(t *testing.T) {
		freshH2cTransport(t)
		freshH2cTransport(t)
	})
	if chromeH2IdleConnTimeout != before || currentChromeH2Transport().IdleConnTimeout != before {
		t.Fatal("fixture must restore both the idle policy and the active transport")
	}
}

func TestRebuildClosesAlreadyIdleConnection(t *testing.T) {
	srv := newH2cServer(t)
	old := freshH2cTransport(t)
	if err := get(srv.url + "/fast"); err != nil {
		t.Fatal(err)
	}
	RebuildChromeTransport()
	waitClosed(t, old.conn(t, 0), time.Second, "idle retired pool")
}

func TestRetiredPoolKeepsActiveRequestThenReclaimsAfterCompletion(t *testing.T) {
	srv := newH2cServer(t)
	old := freshH2cTransport(t)
	done := startHold(srv)
	waitEntered(t, srv)

	RebuildChromeTransport()
	time.Sleep(4 * testH2IdleTimeout) // active well beyond the reclaim period
	assertOpen(t, old.conn(t, 0), "active request on retired pool")
	select {
	case err := <-done:
		t.Fatalf("active request ended early: %v", err)
	default:
	}

	srv.releaseAll()
	waitDone(t, done, "active request")
	waitClosed(t, old.conn(t, 0), 2*time.Second, "retired pool after completion")
}

func TestRebuildRoutesNewRequestsToNewTransportPool(t *testing.T) {
	srv := newH2cServer(t)
	old := freshH2cTransport(t)
	done := startHold(srv)
	waitEntered(t, srv)

	fresh := freshH2cTransport(t)
	if err := get(srv.url + "/fast"); err != nil {
		t.Fatal(err)
	}
	if got := old.dials(); got != 1 {
		t.Fatalf("retired pool dials=%d, want 1 (new request must not reuse it)", got)
	}
	if got := fresh.dials(); got != 1 {
		t.Fatalf("new pool dials=%d, want 1", got)
	}
	assertOpen(t, old.conn(t, 0), "retired pool while its request is active")

	srv.releaseAll()
	waitDone(t, done, "active request")
	waitClosed(t, old.conn(t, 0), 2*time.Second, "retired pool after completion")
	waitClosed(t, fresh.conn(t, 0), 2*time.Second, "new pool after idle")
}

func TestRepeatedRebuildsReclaimEveryRetiredPool(t *testing.T) {
	srv := newH2cServer(t)
	const rounds = 3
	var pools []*h2cPool
	var dones []chan error
	for i := 0; i < rounds; i++ {
		pools = append(pools, freshH2cTransport(t))
		dones = append(dones, startHold(srv))
		waitEntered(t, srv)
	}
	RebuildChromeTransport()
	time.Sleep(3 * testH2IdleTimeout)
	for _, p := range pools {
		assertOpen(t, p.conn(t, 0), "pool with active request")
	}

	srv.releaseAll()
	for _, d := range dones {
		waitDone(t, d, "active request")
	}
	for _, p := range pools {
		waitClosed(t, p.conn(t, 0), 2*time.Second, "retired pool after completion")
	}
}
