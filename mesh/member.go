package mesh

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beego/beego/logs"
	"github.com/gorilla/websocket"
)

const (
	memberStateFile = "member.json"
	tokenPrefix     = "casos1"
)

// Membership is what a member machine keeps about the cloud it joined.
type Membership struct {
	HubURL     string `json:"hubUrl"`
	CAPEM      string `json:"caPem"`
	Machine    string `json:"machine"`
	Credential string `json:"credential"`
	ConsoleURL string `json:"consoleUrl"`
	JoinedTime string `json:"joinedTime"`
}

type JoinRequest struct {
	Invite   string `json:"invite"`
	Secret   string `json:"secret"`
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
}

type JoinResponse struct {
	Machine    string `json:"machine"`
	Credential string `json:"credential"`
	ConsoleURL string `json:"consoleUrl"`
}

// InviteToken is the string an administrator hands to a machine that should
// join. It carries the fingerprint of the hub's CA, so the joining machine can
// authenticate a hub that has no publicly trusted certificate.
func InviteToken(caCert *x509.Certificate, invite, secret string) string {
	return strings.Join([]string{tokenPrefix, strings.TrimPrefix(CAHash(caCert), "sha256:"), invite, secret}, ".")
}

func (h *Hub) InviteToken(invite, secret string) string {
	return InviteToken(h.caCert, invite, secret)
}

func parseInviteToken(token string) (caHash, invite, secret string, err error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 4 || parts[0] != tokenPrefix || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", "", "", fmt.Errorf("this is not a CasOS cloud invite")
	}
	return parts[1], parts[2], parts[3], nil
}

func memberStatePath(dataDir string) string {
	return filepath.Join(dataDir, memberStateFile)
}

// LoadMembership returns the cloud this machine joined, or nil when it has
// joined none.
func LoadMembership(dataDir string) (*Membership, error) {
	data, err := os.ReadFile(memberStatePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m := &Membership{}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("read %s: %w", memberStatePath(dataDir), err)
	}
	return m, nil
}

func saveMembership(dataDir string, m *Membership) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(memberStatePath(dataDir), data, 0o600)
}

func RemoveMembership(dataDir string) error {
	err := os.Remove(memberStatePath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Join makes this machine a member of the cloud at hubURL. It only records
// the membership: the machine becomes a node once CasOS runs as a member and
// the hub deploys it.
func Join(ctx context.Context, cfg Config, hubURL, token, hostname, osName string) (*Membership, error) {
	caHash, invite, secret, err := parseInviteToken(token)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(hubURL), "/"))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("the cloud address must look like https://host:%d", defaultPort)
	}
	hubURL = parsed.String()

	caPEM, err := fetchPinnedCA(ctx, hubURL, caHash)
	if err != nil {
		return nil, err
	}
	client, err := pinnedClient(caPEM)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(JoinRequest{Invite: invite, Secret: secret, Hostname: hostname, OS: osName})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/casos/mesh/join", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the cloud at %s: %w", hubURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the cloud refused to let this machine join: %s", strings.TrimSpace(string(data)))
	}
	var joined JoinResponse
	if err := json.Unmarshal(data, &joined); err != nil || joined.Credential == "" {
		return nil, fmt.Errorf("the cloud sent an unreadable answer")
	}
	m := &Membership{
		HubURL:     hubURL,
		CAPEM:      caPEM,
		Machine:    joined.Machine,
		Credential: joined.Credential,
		ConsoleURL: joined.ConsoleURL,
		JoinedTime: time.Now().Format(time.RFC3339),
	}
	if err := saveMembership(cfg.DataDir, m); err != nil {
		return nil, err
	}
	return m, nil
}

// fetchPinnedCA downloads the hub's CA without trusting the connection, then
// accepts it only if it matches the fingerprint in the invite.
func fetchPinnedCA(ctx context.Context, hubURL, caHash string) (string, error) {
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/casos/mesh/ca", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach the cloud at %s: %w", hubURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	block, _ := pem.Decode(data)
	if resp.StatusCode != http.StatusOK || block == nil {
		return "", fmt.Errorf("%s does not look like a CasOS cloud", hubURL)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("%s sent an unreadable certificate", hubURL)
	}
	if strings.TrimPrefix(CAHash(cert), "sha256:") != caHash {
		return "", fmt.Errorf("%s is not the cloud this invite was made for: its certificate does not match the invite", hubURL)
	}
	return string(data), nil
}

func pinnedTLSConfig(caPEM string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, fmt.Errorf("the cloud's CA is unreadable")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// The hub is addressed directly, never through the local HTTP proxy: the
// connection is pinned to the hub's own CA, which a proxy would break.
func pinnedClient(caPEM string) (*http.Client, error) {
	tlsConfig, err := pinnedTLSConfig(caPEM)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil, TLSClientConfig: tlsConfig}}, nil
}

var memberConnected atomic.Bool

// MemberConnected reports whether this member currently holds its connection
// to the hub.
func MemberConnected() bool { return memberConnected.Load() }

// AgentExecutor runs the hub's requests on this machine's node.
type AgentExecutor func(ctx context.Context, req AgentRequest) (string, error)

// RunAgent keeps this member connected to its hub until ctx ends.
func RunAgent(ctx context.Context, m *Membership, execute AgentExecutor) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := serveAgentOnce(ctx, m, execute)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		logs.Warning("mesh: connection to %s lost, retrying in %s: %v", m.HubURL, backoff, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

func serveAgentOnce(ctx context.Context, m *Membership, execute AgentExecutor) error {
	tlsConfig, err := pinnedTLSConfig(m.CAPEM)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{TLSClientConfig: tlsConfig, HandshakeTimeout: 30 * time.Second}
	wsURL := "wss" + strings.TrimPrefix(m.HubURL, "https") + "/casos/mesh/agent"
	header := http.Header{"Authorization": {"Bearer " + m.Credential}}
	conn, resp, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("the cloud no longer recognises this machine; it may have been removed there")
		}
		return err
	}
	defer conn.Close()
	logs.Info("mesh: connected to %s as %s", m.HubURL, m.Machine)
	memberConnected.Store(true)
	defer memberConnected.Store(false)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-connCtx.Done()
		_ = conn.Close()
	}()
	var writeMu sync.Mutex
	_ = conn.SetReadDeadline(time.Now().Add(agentReadWait))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(agentReadWait))
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(agentWriteWait))
	})
	for {
		var req AgentRequest
		if err := conn.ReadJSON(&req); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(agentReadWait))
		go func(req AgentRequest) {
			reqCtx := connCtx
			if req.TimeoutSeconds > 0 {
				var cancelReq context.CancelFunc
				reqCtx, cancelReq = context.WithTimeout(connCtx, time.Duration(req.TimeoutSeconds)*time.Second)
				defer cancelReq()
			}
			output, err := execute(reqCtx, req)
			resp := AgentResponse{ID: req.ID, Output: output}
			if err != nil {
				resp.Error = err.Error()
			}
			writeMu.Lock()
			defer writeMu.Unlock()
			_ = conn.SetWriteDeadline(time.Now().Add(agentWriteWait))
			_ = conn.WriteJSON(resp)
		}(req)
	}
}

// Leave tells the hub this machine is leaving, so it drops the node.
func Leave(ctx context.Context, m *Membership) error {
	client, err := pinnedClient(m.CAPEM)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.HubURL+"/casos/mesh/leave", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.Credential)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnauthorized {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}
