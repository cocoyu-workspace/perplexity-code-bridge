package netutil

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

func DialThroughProxy(ctx context.Context, network, addr, proxyURL string) (net.Conn, error) {
	base := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if proxyURL == "" {
		return base.DialContext(ctx, network, addr)
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, base)
		if err != nil {
			return nil, fmt.Errorf("socks5 dialer: %w", err)
		}
		if cd, ok := d.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return dialWithCtx(ctx, func() (net.Conn, error) { return d.Dial(network, addr) })
	case "http", "https":
		return httpConnect(ctx, base, u, addr)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %q", u.Scheme)
	}
}

func httpConnect(ctx context.Context, base *net.Dialer, proxyURL *url.URL, target string) (net.Conn, error) {
	conn, err := base.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyURL.Host, err)
	}
	header := make(http.Header)
	header.Set("Host", target)
	header.Set("User-Agent", userAgent)
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		header.Set("Proxy-Authorization", "Basic "+creds)
	}
	req := &http.Request{
		Method: "CONNECT",
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: header,
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
		_ = conn.SetReadDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		conn.Close()
		return nil, fmt.Errorf("CONNECT %s: %s", target, resp.Status)
	}
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
	if br.Buffered() > 0 {
		buf, _ := br.Peek(br.Buffered())
		conn = &prefixConn{Conn: conn, prefix: buf}
	}
	return conn, nil
}

type prefixConn struct {
	net.Conn
	prefix []byte
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

func dialWithCtx(ctx context.Context, dial func() (net.Conn, error)) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dial()
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.c, r.err
	}
}

func ValidateProxyURL(proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h", "http", "https":
		if u.Host == "" {
			return fmt.Errorf("proxy URL %q is missing host", proxyURL)
		}
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme %q (want http/https/socks5)", u.Scheme)
	}
}
