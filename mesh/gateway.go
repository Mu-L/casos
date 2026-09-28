package mesh

import (
	"crypto/tls"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	"github.com/beego/beego/logs"
	derpServer "github.com/juanfont/headscale/hscontrol/derp/server"
)

// servePublicGateway is the hub's one internet-facing listener. The DERP relay
// is served here directly, because a relay connection is a hijacked HTTP/1.1
// stream; the rest of the coordination protocol is proxied to headscale.
func (h *Hub) servePublicGateway(cert tls.Certificate) error {
	target, _ := url.Parse("http://" + h.headscaleAddr)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1

	mux := http.NewServeMux()
	mux.HandleFunc("/derp", h.headscale.DERPServer.DERPHandler)
	mux.HandleFunc("/derp/probe", derpServer.DERPProbeHandler)
	mux.HandleFunc("/derp/latency-check", derpServer.DERPProbeHandler)
	mux.HandleFunc("GET /casos/mesh/ca", h.serveCA)
	mux.HandleFunc("GET /casos/mesh/bin/{arch}", h.serveTailscaleBundle)
	mux.Handle("/casos/mesh/", h.extra)
	mux.Handle("/", proxy)

	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(h.cfg.Port)))
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			// The control and relay protocols upgrade an HTTP/1.1 connection,
			// which HTTP/2 cannot carry.
			NextProtos: []string{"http/1.1"},
			MinVersion: tls.VersionTLS12,
		},
	}
	go func() {
		if err := srv.ServeTLS(listener, "", ""); err != nil && err != http.ErrServerClosed {
			logs.Error("mesh gateway stopped: %v", err)
		}
	}()
	return nil
}

func (h *Hub) serveCA(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.caCert.Raw}))
}
