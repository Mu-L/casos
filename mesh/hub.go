package mesh

import (
	"bufio"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/beego/beego/logs"
	"github.com/juanfont/headscale/hscontrol"
	"tailscale.com/envknob"
	"tailscale.com/logtail"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tsnet"
)

const nodeAuthKeyTTL = time.Hour

// Hub is the running control side of a multi-machine cloud.
type Hub struct {
	cfg           Config
	caCert        *x509.Certificate
	headscale     *hscontrol.Headscale
	headscaleAddr string
	node          *tsnet.Server
	overlayIP     netip.Addr
	egressSocket  string
	extra         *http.ServeMux
}

var current atomic.Pointer[Hub]

// CurrentHub is the hub this process runs, or nil when it is not one.
func CurrentHub() *Hub {
	return current.Load()
}

// StartHub brings the overlay up before the apiserver starts, because the
// apiserver has to advertise the address the hub holds on it: that is the
// only address of the control plane every node can reach.
func StartHub(ctx context.Context, cfg Config, caCert *x509.Certificate, caKey *rsa.PrivateKey) (*Hub, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	cert, leaf, err := ensureGatewayCert(cfg.DataDir, cfg.PublicAddress, caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("mesh gateway certificate: %w", err)
	}
	hs, hsAddr, err := startHeadscale(cfg, leaf)
	if err != nil {
		return nil, err
	}
	h := &Hub{
		cfg:           cfg,
		caCert:        caCert,
		headscale:     hs,
		headscaleAddr: hsAddr,
		extra:         http.NewServeMux(),
	}
	if err := h.servePublicGateway(cert); err != nil {
		return nil, fmt.Errorf("mesh gateway on port %d: %w", cfg.Port, err)
	}
	openHostFirewall(ctx, cfg)
	if err := h.joinOverlay(ctx); err != nil {
		return nil, err
	}
	// The apiserver is advertised at the overlay address, and so is CasOS
	// itself to pods that call back into it (the ACME challenge backend).
	for _, port := range []int{cfg.ApiserverPort, cfg.HTTPPort} {
		if err := h.forwardToHost(port); err != nil {
			return nil, err
		}
	}
	if err := h.serveEgressProxy(); err != nil {
		return nil, err
	}
	current.Store(h)
	logs.Info("mesh hub ready: members join at %s, control plane overlay address %s", cfg.PublicURL(), h.overlayIP)
	return h, nil
}

// joinOverlay puts the hub process itself on the overlay, in userspace, so the
// apiserver can reach kubelets behind NAT without the host needing a TUN
// device or administrator rights.
func (h *Hub) joinOverlay(ctx context.Context) error {
	dir := filepath.Join(h.cfg.DataDir, "tsnet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The client library otherwise uploads its logs to Tailscale Inc., which
	// a self-hosted cloud has no business doing.
	envknob.SetNoLogsNoSupport()
	logtail.Disable()
	h.node = &tsnet.Server{
		Dir:        dir,
		Hostname:   "casos-hub",
		ControlURL: "http://" + h.headscaleAddr,
		Logf:       func(string, ...any) {},
		UserLogf:   func(format string, args ...any) { logs.Info("mesh: "+format, args...) },
	}
	if _, err := os.Stat(filepath.Join(dir, "tailscaled.state")); err != nil {
		key, err := newAuthKey(h.headscale, nodeAuthKeyTTL)
		if err != nil {
			return err
		}
		h.node.AuthKey = key
	}
	upCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := h.node.Up(upCtx); err != nil {
		return fmt.Errorf("join the hub to its own overlay: %w", err)
	}
	ip4, _ := h.node.TailscaleIPs()
	if !ip4.IsValid() {
		return fmt.Errorf("the hub was given no overlay IPv4 address")
	}
	h.overlayIP = ip4
	return nil
}

// forwardToHost answers connections to port on the hub's overlay address
// with the listener on the same port on this host.
func (h *Hub) forwardToHost(port int) error {
	ln, err := h.node.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("listen on overlay port %d: %w", port, err)
	}
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				logs.Warning("mesh forwarder for port %d stopped: %v", port, err)
				return
			}
			go func() {
				upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
				if err != nil {
					conn.Close()
					return
				}
				pipe(conn, upstream)
			}()
		}
	}()
	return nil
}

// serveEgressProxy is where the apiserver sends its traffic to the cluster
// (kubelets for logs, exec and port-forward, and anything it proxies to).
// Overlay addresses are dialled through the hub's overlay node, everything
// else directly, as the apiserver would have done without the proxy.
//
// It listens on a Unix socket because the apiserver speaks plain HTTP CONNECT
// only over one; over TCP it insists on mutual TLS.
func (h *Hub) serveEgressProxy() error {
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("casos-egress-%d.sock", os.Getpid()))
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("egress proxy socket: %w", err)
	}
	h.egressSocket = socket
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				logs.Warning("mesh egress proxy stopped: %v", err)
				return
			}
			go h.handleConnect(conn)
		}
	}()
	return nil
}

func (h *Hub) handleConnect(conn net.Conn) {
	reader := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	req, err := http.ReadRequest(reader)
	if err != nil {
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if req.Method != http.MethodConnect {
		_, _ = io.WriteString(conn, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		conn.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The apiserver sends Host: 127.0.0.1; the target is the request URI.
	upstream, err := h.dial(ctx, req.URL.Host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		conn.Close()
		return
	}
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		conn.Close()
		upstream.Close()
		return
	}
	if n := reader.Buffered(); n > 0 {
		buffered, _ := reader.Peek(n)
		if _, err := upstream.Write(buffered); err != nil {
			conn.Close()
			upstream.Close()
			return
		}
	}
	pipe(conn, upstream)
}

func (h *Hub) dial(ctx context.Context, hostport string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	if addr, err := netip.ParseAddr(host); err == nil && tsaddr.IsTailscaleIP(addr) {
		return h.node.Dial(ctx, "tcp", hostport)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", hostport)
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

// Handle registers an endpoint under /casos/mesh/ on the public gateway.
func (h *Hub) Handle(pattern string, handler http.Handler) {
	h.extra.Handle(pattern, handler)
}

func (h *Hub) Config() Config { return h.cfg }

// OverlayIP is the control plane's address on the overlay: where kubelets
// reach the apiserver and what flannel uses to pick the overlay interface.
func (h *Hub) OverlayIP() netip.Addr { return h.overlayIP }

// EgressProxySocket is the Unix socket of the HTTP CONNECT proxy the
// apiserver sends cluster traffic through.
func (h *Hub) EgressProxySocket() string { return h.egressSocket }

func (h *Hub) CAPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.caCert.Raw}))
}

// NewNodeAuthKey mints a single-use key that lets one node join the overlay.
func (h *Hub) NewNodeAuthKey() (string, error) {
	return newAuthKey(h.headscale, nodeAuthKeyTTL)
}
