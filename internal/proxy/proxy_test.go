package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func httpServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, "hello "+r.URL.Path)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// chain starts remote backend (direct, no auth) + edge frontend (auth, relay).
func chain(t *testing.T) (edge string) {
	old := destinationCheck
	destinationCheck = func(netip.Addr, bool) bool { return true }
	t.Cleanup(func() { destinationCheck = old })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	back := New(Config{Listen: "127.0.0.1:0", AllowClients: []netip.Prefix{netip.MustParsePrefix("10.200.0.0/30")}}, nil)
	if err := back.Listen(ctx, 0); err != nil {
		t.Fatal(err)
	}
	go back.Serve(ctx)
	front := New(Config{Listen: "127.0.0.1:0", Users: map[string]string{"u": "secret"}, Upstream: back.Addr().String()}, nil)
	if err := front.Listen(ctx, 0); err != nil {
		t.Fatal(err)
	}
	go front.Serve(ctx)
	t.Cleanup(func() { front.Close(); back.Close() })
	return front.Addr().String()
}

func socksConnect(t *testing.T, proxy, user, pass, target string) (net.Conn, byte) {
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 2})
	r := make([]byte, 2)
	if _, err := io.ReadFull(c, r); err != nil || r[1] != 2 {
		t.Fatalf("method %v %v", r, err)
	}
	c.Write(append(append(append([]byte{1, byte(len(user))}, user...), byte(len(pass))), pass...))
	if _, err := io.ReadFull(c, r); err != nil {
		t.Fatal(err)
	}
	if r[1] != 0 {
		return c, 0xff
	}
	host, ps, _ := net.SplitHostPort(target)
	var port int
	for _, ch := range ps {
		port = port*10 + int(ch-'0')
	}
	req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
	req = append(req, byte(port>>8), byte(port))
	c.Write(req)
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatal(err)
	}
	return c, rep[1]
}

func TestSOCKS5ThroughRelay(t *testing.T) {
	edge := chain(t)
	target := echoServer(t)
	_, port, _ := net.SplitHostPort(target)
	c, code := socksConnect(t, edge, "u", "secret", "localhost:"+port) // domain resolved by backend
	defer c.Close()
	if code != 0 {
		t.Fatalf("reply %d", code)
	}
	msg := strings.Repeat("ping", 5000)
	go c.Write([]byte(msg))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo mismatch: %v", err)
	}
}

func TestSOCKS5BadPassword(t *testing.T) {
	edge := chain(t)
	c, code := socksConnect(t, edge, "u", "wrong", "127.0.0.1:1")
	c.Close()
	if code != 0xff {
		t.Fatal("bad password accepted")
	}
}

func TestSOCKS5NoAuthRejectedWhenUsersSet(t *testing.T) {
	edge := chain(t)
	c, _ := net.Dial("tcp", edge)
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte{5, 1, 0})
	r := make([]byte, 2)
	io.ReadFull(c, r)
	if r[1] != 0xff {
		t.Fatalf("no-auth accepted: %v", r)
	}
}

func TestHTTPProxy(t *testing.T) {
	edge := chain(t)
	web := httpServer(t)
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("u:secret"))
	// plain request
	c, _ := net.Dial("tcp", edge)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET http://"+web+"/x HTTP/1.1\r\nHost: "+web+"\r\nProxy-Authorization: "+auth+"\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	c.Close()
	if resp.StatusCode != 200 || string(b) != "hello /x" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	// missing auth
	c, _ = net.Dial("tcp", edge)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET http://"+web+"/x HTTP/1.1\r\nHost: "+web+"\r\n\r\n")
	resp, err = http.ReadResponse(bufio.NewReader(c), nil)
	c.Close()
	if err != nil || resp.StatusCode != 407 {
		t.Fatalf("want 407, got %v %v", resp, err)
	}
	// CONNECT tunnel
	echo := echoServer(t)
	c, _ = net.Dial("tcp", edge)
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "CONNECT "+echo+" HTTP/1.1\r\nHost: "+echo+"\r\nProxy-Authorization: "+auth+"\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err = http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("connect: %v %v", resp, err)
	}
	io.WriteString(c, "abc")
	buf := make([]byte, 3)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "abc" {
		t.Fatalf("connect echo %q %v", buf, err)
	}
}

func TestBackendRejectsForeignClients(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(Config{Listen: "127.0.0.1:0", AllowClients: []netip.Prefix{netip.MustParsePrefix("10.200.0.0/30")}}, nil)
	if !s.clientAllowed(&net.TCPAddr{IP: net.ParseIP("10.200.0.1")}) || !s.clientAllowed(&net.TCPAddr{IP: net.ParseIP("127.0.0.1")}) {
		t.Fatal("tunnel/loopback client refused")
	}
	if s.clientAllowed(&net.TCPAddr{IP: net.ParseIP("8.8.8.8")}) {
		t.Fatal("foreign client allowed")
	}
	_ = ctx
}

func TestDestinationAllowed(t *testing.T) {
	for ip, want := range map[string]bool{"1.1.1.1": true, "2606:4700::1111": true, "127.0.0.1": false, "::1": false,
		"169.254.169.254": false, "10.0.0.1": false, "192.168.1.1": false, "100.64.1.1": false, "0.0.0.0": false, "fe80::1": false, "224.0.0.1": false} {
		if got := DestinationAllowed(netip.MustParseAddr(ip), false); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
	if !DestinationAllowed(netip.MustParseAddr("10.0.0.1"), true) {
		t.Error("allow_private ignored")
	}
}

func TestBackendRefusesLoopbackDestination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	back := New(Config{Listen: "127.0.0.1:0"}, nil)
	back.Listen(ctx, 0)
	go back.Serve(ctx)
	defer back.Close()
	_, err := dialSOCKS5(ctx, back.Addr().String(), echoServer(t))
	var ue upstreamError
	if err == nil || !errorsAs(err, &ue) || ue.code != repNotAllowed {
		t.Fatalf("want not-allowed, got %v", err)
	}
}

func errorsAs(err error, target *upstreamError) bool {
	ue, ok := err.(upstreamError)
	if ok {
		*target = ue
	}
	return ok
}
