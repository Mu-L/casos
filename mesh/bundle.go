package mesh

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/beego/beego/logs"
	"github.com/casosorg/casos/conf"
	"github.com/casosorg/casos/proxy"
)

// The newest client release the embedded headscale was built against. A
// newer client usually works too, but this is the pairing that is known to.
const defaultTailscaleVersion = "1.98.4"

var bundleArches = map[string]bool{"amd64": true, "arm64": true, "arm": true}

var bundleLocks sync.Map

// TailscaleVersion is the overlay client release the hub hands to nodes.
func TailscaleVersion() string {
	return conf.GetConfigStringDefault("meshTailscaleVersion", defaultTailscaleVersion)
}

// The hub hands nodes the Tailscale client itself, so a node never needs to
// reach pkgs.tailscale.com: the hub is the one machine every node can
// already reach.
func (h *Hub) serveTailscaleBundle(w http.ResponseWriter, r *http.Request) {
	arch := r.PathValue("arch")
	if !bundleArches[arch] {
		http.Error(w, "unsupported architecture", http.StatusNotFound)
		return
	}
	path, err := h.tailscaleBundle(r.Context(), arch)
	if err != nil {
		logs.Warning("mesh: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, path)
}

func (h *Hub) tailscaleBundle(ctx context.Context, arch string) (string, error) {
	name := fmt.Sprintf("tailscale_%s_%s.tgz", TailscaleVersion(), arch)
	dir := filepath.Join(h.cfg.DataDir, "bin")
	path := filepath.Join(dir, name)

	lock, _ := bundleLocks.LoadOrStore(arch, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	url := "https://pkgs.tailscale.com/stable/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := *proxy.HTTPClient()
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, name+".*.part")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
