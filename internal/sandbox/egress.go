package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Network access for sandboxed commands without sharing the host's network namespace.
//
// With plain "allow network", bwrap used to skip --unshare-net, so sandboxed code could reach
// every service on the host's loopback interface (including the soma dashboard, which would let
// it approve its own actions) and cloud metadata endpoints. Instead, a networked sandbox keeps
// its own empty network namespace and reaches the internet only through an HTTP(S) proxy:
//
//	sandboxed process --HTTP_PROXY--> 127.0.0.1:3128 (forwarder inside the sandbox netns)
//	  --unix socket bind-mounted into the sandbox--> host EgressProxy --> public internet
//
// The host proxy refuses loopback, private, link-local, CGNAT and metadata destinations after
// resolving DNS, so the host itself and the LAN are unreachable. The in-sandbox forwarder is the
// static soma binary itself (ForwarderBinary), so nothing extra is installed.

// ForwarderBinary is the soma executable that can run the in-sandbox forwarder
// (`soma __sandbox-netfwd`). cmd/soma sets it at startup. When empty (tests, library use),
// networked sandboxes fall back to sharing the host network namespace.
var ForwarderBinary string

// ForwarderCommand is the hidden subcommand name handled by cmd/soma before cobra runs.
const ForwarderCommand = "__sandbox-netfwd"

const (
	sandboxProxyAddr = "127.0.0.1:3128"
	egressSandboxDir = "/soma-egress"
	forwarderPath    = "/soma-netfwd"
)

// EgressProxy is the host side: an HTTP proxy (CONNECT and absolute-form requests) on a unix socket.
type EgressProxy struct {
	dir  string
	ln   net.Listener
	srv  *http.Server
	once sync.Once
}

// NewEgressProxy starts an egress proxy on a fresh unix socket.
func NewEgressProxy() (*EgressProxy, error) {
	dir, err := os.MkdirTemp("", "soma-eg")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "egress.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	_ = os.Chmod(dir, 0o755)
	_ = os.Chmod(sock, 0o666)
	p := &EgressProxy{dir: dir, ln: ln}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// SocketPath is the unix socket the sandbox forwarder connects to.
func (p *EgressProxy) SocketPath() string { return filepath.Join(p.dir, "egress.sock") }

// Close stops the proxy.
func (p *EgressProxy) Close() error {
	var err error
	p.once.Do(func() {
		err = p.srv.Close()
		_ = os.RemoveAll(p.dir)
	})
	return err
}

// ErrBlockedDestination is returned for loopback, private, link-local and metadata targets.
var ErrBlockedDestination = errors.New("sandbox egress: destination is not a public internet address")

// BlockedEgressIP reports addresses a sandbox must never reach through the proxy.
func BlockedEgressIP(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0, // 0.0.0.0/8
			v4[0] == 100 && v4[1]&0xc0 == 64,             // CGNAT 100.64.0.0/10
			v4[0] == 198 && (v4[1] == 18 || v4[1] == 19), // benchmarking 198.18.0.0/15
			v4[0] >= 240: // reserved + broadcast
			return true
		}
		return false
	}
	// NAT64 (64:ff9b::/96) and 6to4 (2002::/16) can embed private IPv4 targets.
	if (ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b) || (ip[0] == 0x20 && ip[1] == 0x02) {
		return true
	}
	return false
}

// safeDial resolves host and dials the first public address, refusing blocked targets.
func safeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if BlockedEgressIP(ip.IP) {
			return nil, fmt.Errorf("%w (%s -> %s)", ErrBlockedDestination, host, ip.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("sandbox egress: %s has no addresses", host)
	}
	d := net.Dialer{Timeout: 30 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

var egressTransport = &http.Transport{
	DialContext:           safeDial,
	Proxy:                 nil,
	ResponseHeaderTimeout: 5 * time.Minute,
}

func (p *EgressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		upstream, err := safeDial(r.Context(), "tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		client, buf, err := hj.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() {
			if buf.Reader.Buffered() > 0 {
				_, _ = io.CopyN(upstream, buf, int64(buf.Reader.Buffered()))
			}
			_, _ = io.Copy(upstream, client)
			_ = upstream.Close()
		}()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		return
	}
	if !r.URL.IsAbs() {
		http.Error(w, "sandbox egress proxy: absolute URL required", http.StatusBadRequest)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header.Del("Proxy-Connection")
	out.Header.Del("Proxy-Authorization")
	resp, err := egressTransport.RoundTrip(out)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, ErrBlockedDestination) {
			code = http.StatusForbidden
		}
		http.Error(w, err.Error(), code)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// RunNetForwarder is the in-sandbox side: args are <listen-addr> <unix-socket> -- <command...>.
// It forwards TCP connections on listen-addr to the unix socket while running the command, and
// returns the command's exit code.
func RunNetForwarder(args []string) int {
	if len(args) < 4 || args[2] != "--" {
		fmt.Fprintln(os.Stderr, "usage: soma __sandbox-netfwd <listen> <unix-socket> -- <command...>")
		return 2
	}
	listen, sock, cmdArgs := args[0], args[1], args[3:]
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sandbox egress forwarder: %v\n", err)
		return 125
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				u, err := net.Dial("unix", sock)
				if err != nil {
					_ = c.Close()
					return
				}
				go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
				_, _ = io.Copy(c, u)
				_ = c.Close()
			}(c)
		}
	}()
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
		return 127
	}
	return 0
}

// proxyEnv is the environment that routes a sandboxed process through the forwarder.
func proxyEnv() map[string]string {
	u := "http://" + sandboxProxyAddr
	return map[string]string{
		"HTTP_PROXY": u, "HTTPS_PROXY": u, "http_proxy": u, "https_proxy": u, "ALL_PROXY": u,
		"NO_PROXY": "localhost,127.0.0.1", "no_proxy": "localhost,127.0.0.1",
		"SOMA_SANDBOX_PROXY_PORT": strconv.Itoa(3128),
	}
}
