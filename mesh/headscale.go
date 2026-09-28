package mesh

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/beego/beego/logs"
	"github.com/juanfont/headscale/hscontrol"
	hstypes "github.com/juanfont/headscale/hscontrol/types"
	"gopkg.in/yaml.v3"
	"tailscale.com/tailcfg"
)

const (
	derpRegionID = 900
	// Every node and the hub itself belong to this one headscale user; the
	// overlay is a single trust domain, the same as the cluster on top of it.
	headscaleUser = "casos"
)

// startHeadscale runs the coordination server on a loopback port. It never
// faces the internet directly: the gateway terminates TLS in front of it.
func startHeadscale(cfg Config, gatewayLeaf *x509.Certificate) (*hscontrol.Headscale, string, error) {
	port, err := freeLoopbackPort()
	if err != nil {
		return nil, "", err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))

	derpPath := filepath.Join(cfg.DataDir, "derp.yaml")
	if err := writeDERPMap(derpPath, cfg, gatewayLeaf); err != nil {
		return nil, "", err
	}
	configPath := filepath.Join(cfg.DataDir, "headscale.yaml")
	if err := os.WriteFile(configPath, headscaleConfig(cfg, addr, derpPath), 0o600); err != nil {
		return nil, "", err
	}
	if err := hstypes.LoadConfig(configPath, true); err != nil {
		return nil, "", fmt.Errorf("load headscale config: %w", err)
	}
	hsCfg, err := hstypes.LoadServerConfig()
	if err != nil {
		return nil, "", fmt.Errorf("headscale config: %w", err)
	}
	hs, err := hscontrol.NewHeadscale(hsCfg)
	if err != nil {
		return nil, "", fmt.Errorf("start headscale: %w", err)
	}
	go func() {
		if err := hs.Serve(); err != nil {
			logs.Error("mesh coordination server exited: %v", err)
		}
	}()
	if err := waitForHTTP(fmt.Sprintf("http://%s/health", addr), 30*time.Second); err != nil {
		return nil, "", fmt.Errorf("mesh coordination server did not start: %w", err)
	}
	return hs, addr, nil
}

// The key and database paths are relative: headscale resolves them against
// the config file's directory, and treats any path not starting with "/" as
// relative, a Windows drive path included.
func headscaleConfig(cfg Config, listenAddr, derpPath string) []byte {
	_, port, _ := net.SplitHostPort(listenAddr)
	doc := map[string]any{
		"server_url":            cfg.PublicURL(),
		"listen_addr":           listenAddr,
		"metrics_listen_addr":   "",
		"grpc_listen_addr":      "127.0.0.1:0",
		"disable_check_updates": true,
		"noise":                 map[string]any{"private_key_path": "noise_private.key"},
		"prefixes":              map[string]any{"v4": "100.64.0.0/10", "allocation": "sequential"},
		"derp": map[string]any{
			"server": map[string]any{
				"enabled":                                true,
				"region_id":                              derpRegionID,
				"region_code":                            "casos",
				"region_name":                            "CasOS hub",
				"stun_listen_addr":                       net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.StunPort)),
				"private_key_path":                       "derp_server_private.key",
				"automatically_add_embedded_derp_region": false,
				"verify_clients":                         true,
			},
			"urls":                []string{},
			"paths":               []string{derpPath},
			"auto_update_enabled": false,
		},
		"database": map[string]any{
			"type":   "sqlite",
			"sqlite": map[string]any{"path": "headscale.db", "write_ahead_log": true},
		},
		// Nodes keep their own resolvers: cluster DNS is wired up by the node
		// deployment, and a coordination server rewriting resolv.conf under it
		// would break image pulls.
		"dns": map[string]any{"magic_dns": false, "override_local_dns": false},
		// Windows caps a Unix socket path at 108 bytes, which a data directory
		// under the user profile easily exceeds.
		"unix_socket":            filepath.Join(os.TempDir(), "casos-headscale-"+port+".sock"),
		"unix_socket_permission": "0770",
		"log":                    map[string]any{"level": "warn"},
		"policy":                 map[string]any{"mode": "file", "path": ""},
		"logtail":                map[string]any{"enabled": false},
		"taildrop":               map[string]any{"enabled": false},
	}
	out, _ := yaml.Marshal(doc)
	return out
}

// writeDERPMap publishes the hub as the only relay. Nodes pin its certificate
// by hash, so a hub with no domain and no public CA is still authenticated.
func writeDERPMap(path string, cfg Config, leaf *x509.Certificate) error {
	node := &tailcfg.DERPNode{
		Name:     strconv.Itoa(derpRegionID) + "a",
		RegionID: derpRegionID,
		HostName: cfg.PublicAddress,
		DERPPort: cfg.Port,
		STUNPort: cfg.StunPort,
		CertName: "sha256-raw:" + certRawHash(leaf),
	}
	if ip := net.ParseIP(cfg.PublicAddress); ip != nil {
		if ip.To4() != nil {
			node.IPv4 = ip.String()
		} else {
			node.IPv6 = ip.String()
		}
	}
	derpMap := tailcfg.DERPMap{Regions: map[int]*tailcfg.DERPRegion{
		derpRegionID: {
			RegionID:   derpRegionID,
			RegionCode: "casos",
			RegionName: "CasOS hub",
			Nodes:      []*tailcfg.DERPNode{node},
		},
	}}
	out, err := yaml.Marshal(&derpMap)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func ensureHeadscaleUser(hs *hscontrol.Headscale) (*hstypes.User, error) {
	state := hs.GetState()
	if user, err := state.GetUserByName(headscaleUser); err == nil && user != nil {
		return user, nil
	}
	// No node belongs to a user that has just been created, so there is no
	// change to push to anyone.
	user, _, err := state.CreateUser(hstypes.User{Name: headscaleUser})
	if err != nil {
		return nil, fmt.Errorf("create mesh user: %w", err)
	}
	return user, nil
}

// newAuthKey mints a single-use key that lets one node join the overlay.
func newAuthKey(hs *hscontrol.Headscale, ttl time.Duration) (string, error) {
	user, err := ensureHeadscaleUser(hs)
	if err != nil {
		return "", err
	}
	expiry := time.Now().Add(ttl)
	key, err := hs.GetState().CreatePreAuthKey(user.TypedID(), false, false, &expiry, nil)
	if err != nil {
		return "", fmt.Errorf("create mesh auth key: %w", err)
	}
	return key.Key, nil
}

func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitForHTTP(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr error = errors.New("no response")
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
			lastErr = fmt.Errorf("status %s", resp.Status)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// RemoveNode takes the overlay node named hostname off the overlay.
func (h *Hub) RemoveNode(hostname string) error {
	state := h.headscale.GetState()
	for _, node := range state.ListNodes().All() {
		if node.Hostname() != hostname && node.GivenName() != hostname {
			continue
		}
		c, err := state.DeleteNode(node)
		if err != nil {
			return err
		}
		h.headscale.Change(c)
	}
	return nil
}
