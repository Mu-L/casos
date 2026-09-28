package deploy

import (
	"context"
	"fmt"
	"sync"

	"github.com/beego/beego/logs"
	"github.com/casosorg/casos/mesh"
	"github.com/casosorg/casos/object"
)

// memberNode is the environment a member's node runs in: the host itself on
// Linux, the WSL distribution on Windows. Preparing it can install WSL, so it
// happens once, on the hub's first request.
type memberNode struct {
	mu      sync.Mutex
	machine *NodeDeployMachine
}

func (n *memberNode) runner(ctx context.Context) (NodeDeployRunner, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.machine == nil {
		enrolled, err := localNodeMachine(ctx)
		if err != nil {
			return nil, fmt.Errorf("prepare this machine's node environment: %w", err)
		}
		// The enrollment hands back a copy without the SSH key it stored.
		machine, err := object.GetMachine(enrolled.Owner + "/" + enrolled.Name)
		if err != nil || machine == nil {
			return nil, fmt.Errorf("read machine %s/%s: %v", enrolled.Owner, enrolled.Name, err)
		}
		deployMachine, err := toNodeDeployMachine(machine)
		if err != nil {
			return nil, err
		}
		n.machine = &deployMachine
	}
	return newRunnerForMachine(*n.machine)
}

func (n *memberNode) execute(ctx context.Context, req mesh.AgentRequest) (string, error) {
	runner, err := n.runner(ctx)
	if err != nil {
		return "", err
	}
	defer runner.Close()
	switch req.Op {
	case mesh.AgentOpRun:
		return runner.RunContext(ctx, req.Command)
	case mesh.AgentOpRunRoot:
		return runner.RunRootContext(ctx, req.Command)
	case mesh.AgentOpWriteFile:
		return "", runner.WriteFileContext(ctx, req.Path, req.Content, req.Mode)
	default:
		return "", fmt.Errorf("unknown request %q: this CasOS may be older than the hub", req.Op)
	}
}

// RunMeshMember keeps this machine connected to the hub of the cloud it
// joined, running what the hub asks for on its node, until ctx ends.
func RunMeshMember(ctx context.Context, m *mesh.Membership) {
	node := &memberNode{}
	logs.Info("mesh: this machine is %s in the cloud at %s", m.Machine, m.HubURL)
	// Prepared up front rather than on the hub's first request: an already
	// deployed node gets no request after a restart, and on Windows preparing
	// it is also what keeps its WSL distribution running.
	go func() {
		runner, err := node.runner(ctx)
		if err != nil {
			logs.Error("mesh: %v", err)
			return
		}
		runner.Close()
	}()
	mesh.RunAgent(ctx, m, node.execute)
}

// LeaveMeshCloud takes this machine out of the cloud it joined: the hub drops
// the node, the node's services stop, and the membership is forgotten. With
// force the local cleanup goes ahead even when the hub cannot be told.
func LeaveMeshCloud(ctx context.Context, cfg mesh.Config, m *mesh.Membership, force bool) error {
	if err := mesh.Leave(ctx, m); err != nil {
		if !force {
			return fmt.Errorf("tell the cloud this machine is leaving: %w", err)
		}
		logs.Warning("mesh: leaving without telling %s: %v", m.HubURL, err)
	}
	node := &memberNode{}
	if runner, err := node.runner(ctx); err != nil {
		logs.Warning("mesh: stop node services: %v", err)
	} else {
		cmd := fmt.Sprintf(`%[1]s/tailscale --socket=%[2]s logout >/dev/null 2>&1 || true
systemctl disable --now kubelet kube-proxy casos-mesh >/dev/null 2>&1 || true
rm -f %[3]s %[4]s
update-ca-certificates >/dev/null 2>&1 || true
systemctl daemon-reload`, meshBinDir, meshSocket, meshCAPath, meshServicePath)
		if _, err := runner.RunRootContext(ctx, cmd); err != nil {
			logs.Warning("mesh: stop node services: %v", err)
		}
		runner.Close()
	}
	return mesh.RemoveMembership(cfg.DataDir)
}
