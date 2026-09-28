package mesh

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/casosorg/casos/conf"
)

// Role is the part a CasOS instance plays in a multi-machine cloud.
type Role string

const (
	// RoleStandalone is a single-machine cluster with no overlay network.
	RoleStandalone Role = "standalone"
	// RoleHub runs the control plane of a cloud other machines join over the
	// internet: the coordination server, the relay and the apiserver.
	RoleHub Role = "hub"
	// RoleMember contributes its machine as a worker node of a hub's cluster
	// and runs no control plane of its own.
	RoleMember Role = "member"
)

const (
	// Next to the apiserver's 20443 in the block CasOS reserves for its fixed
	// listeners; this is the one port a hub has to expose to the internet.
	defaultPort     = 20444
	defaultStunPort = 3478
)

type Config struct {
	Role Role
	// PublicAddress is the host name or IP address members reach the hub at.
	PublicAddress string
	// Port is the TLS port that carries the coordination protocol, the relay
	// and the join API.
	Port int
	// StunPort is the UDP port that lets nodes discover their public address,
	// which is what makes direct connections through NAT possible.
	StunPort      int
	DataDir       string
	ApiserverPort int
	HTTPPort      int
}

func ConfigFromAppConf(dataDir string, apiserverPort int) (Config, error) {
	cfg := Config{
		Role:          Role(strings.ToLower(strings.TrimSpace(conf.GetConfigStringDefault("meshRole", string(RoleStandalone))))),
		PublicAddress: strings.TrimSpace(conf.GetConfigString("meshPublicAddress")),
		Port:          conf.GetConfigIntDefault("meshPort", defaultPort),
		StunPort:      conf.GetConfigIntDefault("meshStunPort", defaultStunPort),
		DataDir:       filepath.Join(dataDir, "mesh"),
		ApiserverPort: apiserverPort,
		HTTPPort:      conf.GetConfigIntDefault("httpport", 20080),
	}
	// Joining a cloud or turning the hub on from the web UI records a state
	// file instead of editing app.conf, so that decides the role unless
	// app.conf names one.
	if strings.TrimSpace(conf.GetConfigString("meshRole")) == "" {
		if _, err := os.Stat(memberStatePath(cfg.DataDir)); err == nil {
			cfg.Role = RoleMember
		} else if hub, err := loadHubSettings(cfg.DataDir); err != nil {
			return cfg, err
		} else if hub != nil {
			cfg.Role = RoleHub
			if cfg.PublicAddress == "" {
				cfg.PublicAddress = hub.PublicAddress
			}
		}
	}
	switch cfg.Role {
	case "":
		cfg.Role = RoleStandalone
	case RoleStandalone, RoleMember:
	case RoleHub:
		if cfg.PublicAddress == "" {
			return cfg, fmt.Errorf("meshRole is hub but meshPublicAddress is empty: set it to the IP address or host name other machines reach this one at")
		}
		if strings.Contains(cfg.PublicAddress, "/") || strings.Contains(cfg.PublicAddress, ":") && net.ParseIP(cfg.PublicAddress) == nil {
			return cfg, fmt.Errorf("meshPublicAddress must be a bare host name or IP address, got %q", cfg.PublicAddress)
		}
	default:
		return cfg, fmt.Errorf("meshRole must be standalone, hub or member, got %q", cfg.Role)
	}
	return cfg, nil
}

// ConsoleURL is where the hub's web console is, as a member's owner would
// open it.
func (c Config) ConsoleURL() string {
	return "http://" + net.JoinHostPort(c.PublicAddress, strconv.Itoa(c.HTTPPort)) + "/"
}

// PublicURL is the address of the hub's TLS port as members dial it.
func (c Config) PublicURL() string {
	return "https://" + net.JoinHostPort(c.PublicAddress, strconv.Itoa(c.Port))
}

const hubStateFile = "hub.json"

type hubSettings struct {
	PublicAddress string `json:"publicAddress"`
}

func loadHubSettings(dataDir string) (*hubSettings, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, hubStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	settings := &hubSettings{}
	if err := json.Unmarshal(data, settings); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dataDir, hubStateFile), err)
	}
	return settings, nil
}

// EnableHub makes this CasOS a hub from its next start, reachable by members
// at publicAddress.
func EnableHub(cfg Config, publicAddress string) error {
	publicAddress = strings.TrimSpace(publicAddress)
	if publicAddress == "" || strings.Contains(publicAddress, "/") || strings.Contains(publicAddress, ":") && net.ParseIP(publicAddress) == nil {
		return fmt.Errorf("enter the IP address or host name other machines reach this one at, without a scheme or port")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(hubSettings{PublicAddress: publicAddress}, "", "  ")
	return os.WriteFile(filepath.Join(cfg.DataDir, hubStateFile), data, 0o600)
}
