package mesh

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/beego/beego/logs"
)

// openHostFirewall lets members in from anywhere, not only the local subnets
// the apiserver rule allows: they are expected to be across the internet.
func openHostFirewall(ctx context.Context, cfg Config) {
	if runtime.GOOS != "windows" {
		return
	}
	rules := []struct {
		protocol string
		port     int
		purpose  string
	}{
		{"TCP", cfg.Port, "the CasOS cloud coordination server and relay"},
		{"UDP", cfg.StunPort, "CasOS cloud NAT traversal (STUN)"},
	}
	for _, rule := range rules {
		name := fmt.Sprintf("CasOS mesh %s %d", rule.protocol, rule.port)
		if _, err := netsh(ctx, "show", "rule", "name="+name, "dir=in"); err == nil {
			continue
		}
		output, err := netsh(ctx, "add", "rule",
			"name="+name,
			"dir=in",
			"action=allow",
			"protocol="+rule.protocol,
			fmt.Sprintf("localport=%d", rule.port),
			"profile=any",
			"description=Added by CasOS for "+rule.purpose,
		)
		if err != nil {
			logs.Warning("could not open inbound %s %d in Windows Firewall, so machines on other networks cannot join: %v: %s. Start CasOS as an administrator once, or run in an elevated terminal: netsh advfirewall firewall add rule name=\"%s\" dir=in action=allow protocol=%s localport=%d",
				rule.protocol, rule.port, err, output, name, rule.protocol, rule.port)
			continue
		}
		logs.Info("added Windows Firewall rule %q", name)
	}
}

func netsh(ctx context.Context, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(runCtx, "netsh", append([]string{"advfirewall", "firewall"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}
