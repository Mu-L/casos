package deploy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/casosorg/casos/mesh"
)

// The overlay client lives under its own names so it can sit next to nothing
// else: a machine already on the owner's own tailnet would otherwise have its
// tailscaled replaced and its state taken over.
const (
	meshBinDir      = "/usr/local/lib/casos-mesh"
	meshSocket      = "/run/casos-mesh/tailscaled.sock"
	meshInterface   = "casosmesh0"
	meshCAPath      = "/usr/local/share/ca-certificates/casos-mesh.crt"
	meshServicePath = "/etc/systemd/system/casos-mesh.service"
)

// meshApiserverURL is where every node reaches the apiserver on a mesh: the
// hub's own overlay address, the one address of the control plane that is
// the same from everywhere.
func meshApiserverURL(hub *mesh.Hub, port int) string {
	return "https://" + net.JoinHostPort(hub.OverlayIP().String(), strconv.Itoa(port))
}

// installMeshOverlay puts the node on the hub's overlay and returns the
// address it was given there, which becomes the node's InternalIP.
func (d *NodeDeployer) installMeshOverlay(ctx context.Context, runner NodeDeployRunner, hub *mesh.Hub, nodeName, arch, apiserverURL string) (string, error) {
	cfg := hub.Config()
	d.logStep(nodeDeployPhaseInstalling, fmt.Sprintf("Joining the CasOS cloud overlay at %s", cfg.PublicURL()))

	if _, err := runner.RunRootContext(ctx, "! systemctl is-active --quiet tailscaled"); err != nil {
		return "", fmt.Errorf("this machine already runs Tailscale (tailscaled.service), which would fight the CasOS overlay over routes and firewall marks: stop and disable it, then deploy again")
	}
	if err := runner.WriteFileContext(ctx, meshCAPath, hub.CAPEM(), "0644"); err != nil {
		return "", fmt.Errorf("write %s: %w", meshCAPath, err)
	}
	// The coordination server presents a certificate from the cluster CA; the
	// client trusts it through the system store.
	if _, err := runner.RunRootContext(ctx, "update-ca-certificates >/dev/null"); err != nil {
		return "", fmt.Errorf("trust the cluster CA: %w", err)
	}

	version := mesh.TailscaleVersion()
	installCmd := fmt.Sprintf(`set -e
if [ "$(%[1]s/tailscale version 2>/dev/null | head -n1)" != %[2]s ]; then
  curl -fsSL --connect-timeout 20 --max-time 900 --retry 2 --retry-delay 5 --cacert %[3]s -o /tmp/casos-mesh.tgz %[4]s
  rm -rf /tmp/casos-mesh && mkdir -p /tmp/casos-mesh
  tar -xzf /tmp/casos-mesh.tgz -C /tmp/casos-mesh --strip-components=1
  install -d %[1]s
  install -m 0755 /tmp/casos-mesh/tailscale /tmp/casos-mesh/tailscaled %[1]s/
  rm -rf /tmp/casos-mesh /tmp/casos-mesh.tgz
fi`, meshBinDir, shellSingleQuote(version), meshCAPath, shellSingleQuote(cfg.PublicURL()+"/casos/mesh/bin/"+arch))
	if _, err := runner.RunRootContext(ctx, installCmd); err != nil {
		return "", fmt.Errorf("install the overlay client: %w", err)
	}
	if err := runner.WriteFileContext(ctx, meshServicePath, meshService(), "0644"); err != nil {
		return "", fmt.Errorf("write %s: %w", meshServicePath, err)
	}
	if _, err := runner.RunRootContext(ctx, "modprobe tun 2>/dev/null; systemctl daemon-reload && systemctl enable casos-mesh && systemctl restart casos-mesh"); err != nil {
		return "", fmt.Errorf("start the overlay client: %w", err)
	}

	key, err := hub.NewNodeAuthKey()
	if err != nil {
		return "", err
	}
	up := fmt.Sprintf("%s/tailscale --socket=%s up --login-server=%s --authkey=%s --hostname=%s --accept-dns=false --accept-routes=false --timeout=120s",
		meshBinDir, meshSocket, shellSingleQuote(cfg.PublicURL()), shellSingleQuote(key), shellSingleQuote(nodeName))
	// A node moved to a different hub still holds its old identity, and the
	// client refuses to switch servers without logging out first.
	cmd := fmt.Sprintf(`for i in $(seq 1 30); do [ -S %[1]s ] && break; sleep 1; done
out=$(%[2]s 2>&1) && exit 0
case "$out" in
  *force-reauth*) %[3]s/tailscale --socket=%[1]s logout >/dev/null 2>&1 || true; %[2]s ;;
  *) echo "$out" >&2; exit 1 ;;
esac`, meshSocket, up, meshBinDir)
	if _, err := runner.RunRootContext(ctx, cmd); err != nil {
		return "", fmt.Errorf("join the overlay: %w", err)
	}

	out, err := runner.RunRootContext(ctx, fmt.Sprintf("%s/tailscale --socket=%s ip -4", meshBinDir, meshSocket))
	if err != nil {
		return "", fmt.Errorf("read the overlay address: %w", err)
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]))
	if err != nil || !ip.Is4() {
		return "", fmt.Errorf("the overlay gave this node no IPv4 address (got %q)", strings.TrimSpace(out))
	}
	d.logStep(nodeDeployPhaseInstalling, fmt.Sprintf("On the overlay as %s", ip))

	if err := d.waitForMeshApiserver(ctx, runner, apiserverURL, cfg); err != nil {
		return "", err
	}
	return ip.String(), nil
}

// waitForMeshApiserver gives the overlay time to find a path to the hub,
// direct or through the relay, before the kubelet is pointed at it.
func (d *NodeDeployer) waitForMeshApiserver(ctx context.Context, runner NodeDeployRunner, apiserverURL string, cfg mesh.Config) error {
	deadline := time.Now().Add(90 * time.Second)
	command := nodeDeployApiserverProbeCommand(apiserverURL, nodeDeployPreflightProbeConnectTimeout, nodeDeployPreflightProbeMaxTime)
	var lastErr error
	for {
		status, err := runner.RunContext(ctx, command)
		if err == nil && isNodeDeployApiserverProbeStatus(status) {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("HTTP status %q", strings.TrimSpace(status))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the apiserver at %s is not reachable over the overlay: %w. Check that UDP %d and TCP %d reach the hub, or that the hub's relay is reachable",
				apiserverURL, lastErr, cfg.StunPort, cfg.Port)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func meshService() string {
	// Traffic that reaches a pod on another node from outside the pod network
	// (the Windows host, through its route into WSL) would come back to an
	// address only this node can reach, so it leaves over VXLAN masqueraded.
	masquerade := "iptables -t nat -C POSTROUTING -o flannel.1 ! -s " + nodeDeployClusterCIDR + " -j MASQUERADE 2>/dev/null || iptables -t nat -I POSTROUTING -o flannel.1 ! -s " + nodeDeployClusterCIDR + " -j MASQUERADE"
	return fmt.Sprintf(`[Unit]
Description=CasOS cloud overlay
Wants=network-pre.target
After=network-pre.target systemd-resolved.service
Before=kubelet.service

[Service]
ExecStart=%[1]s/tailscaled --state=/var/lib/casos-mesh/tailscaled.state --socket=%[2]s --tun=%[3]s --port=41641 --no-logs-no-support
ExecStartPost=/bin/sh -c '%[4]s'
RuntimeDirectory=casos-mesh
StateDirectory=casos-mesh
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
`, meshBinDir, meshSocket, meshInterface, masquerade)
}
