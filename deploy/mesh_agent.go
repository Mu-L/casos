package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/beego/beego/logs"
	"github.com/casosorg/casos/mesh"
	"github.com/casosorg/casos/object"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const meshMemberOwner = "admin"

// RegisterMeshEndpoints serves the part of the join protocol that needs the
// machine records: joining, the agent connection and leaving.
func RegisterMeshEndpoints(hub *mesh.Hub) {
	hub.Handle("POST /casos/mesh/join", http.HandlerFunc(serveMeshJoin))
	hub.Handle("GET /casos/mesh/agent", http.HandlerFunc(serveMeshAgent))
	hub.Handle("POST /casos/mesh/leave", http.HandlerFunc(serveMeshLeave))
}

func serveMeshJoin(w http.ResponseWriter, r *http.Request) {
	var req mesh.JoinRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "unreadable join request", http.StatusBadRequest)
		return
	}
	invite, err := object.ConsumeMeshInvite(req.Invite, req.Secret)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	machine, err := addMeshMachine(invite.Owner, req.Hostname, req.OS)
	if err != nil {
		logs.Warning("mesh join from %s: %v", req.Hostname, err)
		http.Error(w, "the cloud could not record this machine", http.StatusInternalServerError)
		return
	}
	credential, err := object.AddMeshMember(machine.Owner, machine.Name, req.Hostname)
	if err != nil {
		_, _ = object.DeleteMachine(machine)
		http.Error(w, "the cloud could not record this machine", http.StatusInternalServerError)
		return
	}
	logs.Info("mesh: %s joined as machine %s/%s with invite %s", req.Hostname, machine.Owner, machine.Name, invite.Name)
	w.Header().Set("Content-Type", "application/json")
	response := mesh.JoinResponse{Machine: machine.Owner + "/" + machine.Name, Credential: credential}
	if hub := mesh.CurrentHub(); hub != nil {
		response.ConsoleURL = hub.Config().ConsoleURL()
	}
	_ = json.NewEncoder(w).Encode(response)
}

// addMeshMachine names the machine after its host, adding a suffix when two
// joined machines share a hostname.
func addMeshMachine(owner, hostname, osName string) (*object.Machine, error) {
	base := sanitizeMachineName(hostname, "member")
	if len(base) > 90 {
		base = strings.TrimRight(base[:90], "-")
	}
	for i := 1; i < 100; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		existing, err := object.GetMachine(owner + "/" + name)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			continue
		}
		machine := &object.Machine{
			Owner:       owner,
			Name:        name,
			CreatedTime: time.Now().UTC().Format(time.RFC3339),
			DisplayName: hostname,
			AuthType:    object.MachineAuthTypeAgent,
			Os:          osName,
			Status:      "Online",
			Description: "Joined this cloud with an invite",
		}
		if _, err := object.AddMachine(machine); err != nil {
			return nil, err
		}
		return machine, nil
	}
	return nil, fmt.Errorf("too many machines named %s", base)
}

func meshMemberFromRequest(r *http.Request) (*object.MeshMember, error) {
	credential, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, nil
	}
	return object.VerifyMeshMemberCredential(strings.TrimSpace(credential))
}

func serveMeshAgent(w http.ResponseWriter, r *http.Request) {
	member, err := meshMemberFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if member == nil {
		http.Error(w, "unknown machine", http.StatusUnauthorized)
		return
	}
	id := member.Owner + "/" + member.Name
	session, err := mesh.AcceptAgent(w, r, id)
	if err != nil {
		return
	}
	object.TouchMeshMember(member)
	logs.Info("mesh: machine %s connected", id)
	go func() {
		<-session.Done()
		object.TouchMeshMember(member)
		logs.Info("mesh: machine %s disconnected", id)
	}()
	deployMeshMemberIfNeeded(member)
}

// deployMeshMemberIfNeeded turns a newly joined machine into a node as soon
// as it connects. A machine that is already a node is left alone on
// reconnect; Repair on the Machines page redeploys it on request.
func deployMeshMemberIfNeeded(member *object.MeshMember) {
	machine, err := object.GetMachine(member.Owner + "/" + member.Name)
	if err != nil || machine == nil {
		return
	}
	if machine.Status == object.MachineStatusDeployed || machine.Status == object.MachineStatusDeploying {
		return
	}
	task, err := defaultService.DeployMachineNode(MachineNodeDeployRequest{
		Owner:       machine.Owner,
		MachineName: machine.Name,
		NodeName:    machine.Name,
	})
	if err != nil {
		logs.Warning("mesh: deploy node on %s/%s: %v", machine.Owner, machine.Name, err)
		return
	}
	logs.Info("mesh: deploying node %s as task %d", machine.Name, task.Id)
}

func serveMeshLeave(w http.ResponseWriter, r *http.Request) {
	member, err := meshMemberFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if member == nil {
		http.Error(w, "unknown machine", http.StatusUnauthorized)
		return
	}
	if err := RemoveMeshMember(r.Context(), member.Owner, member.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// RemoveMeshMember forgets a joined machine: its Kubernetes node, its place on
// the overlay, its credential and its machine record.
func RemoveMeshMember(ctx context.Context, owner, name string) error {
	if restConfig, err := defaultService.restConfigSnapshot(); err == nil {
		if client, err := kubernetes.NewForConfig(restConfig); err == nil {
			err = client.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete node %s: %w", name, err)
			}
		}
	}
	if hub := mesh.CurrentHub(); hub != nil {
		if err := hub.RemoveNode(name); err != nil {
			logs.Warning("mesh: remove %s from the overlay: %v", name, err)
		}
	}
	if _, err := object.DeleteMeshMember(owner, name); err != nil {
		return err
	}
	mesh.DropAgent(owner + "/" + name)
	if _, err := object.DeleteMachine(&object.Machine{Owner: owner, Name: name}); err != nil {
		return err
	}
	logs.Info("mesh: machine %s/%s left the cloud", owner, name)
	return nil
}

// AgentRunner runs node deployment commands on a member machine through the
// connection it holds open to the hub.
type AgentRunner struct {
	id string
}

func NewAgentRunner(id string) (*AgentRunner, error) {
	if mesh.Agent(id) == nil {
		return nil, fmt.Errorf("%s: %w", id, mesh.ErrAgentOffline)
	}
	return &AgentRunner{id: id}, nil
}

func (r *AgentRunner) call(ctx context.Context, req mesh.AgentRequest) (string, error) {
	session := mesh.Agent(r.id)
	if session == nil {
		return "", fmt.Errorf("%s: %w", r.id, mesh.ErrAgentOffline)
	}
	resp, err := session.Call(ctx, req)
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return resp.Output, fmt.Errorf("%s", resp.Error)
	}
	return resp.Output, nil
}

func (r *AgentRunner) RunContext(ctx context.Context, command string) (string, error) {
	return r.call(ctx, mesh.AgentRequest{Op: mesh.AgentOpRun, Command: command})
}

func (r *AgentRunner) RunRootContext(ctx context.Context, command string) (string, error) {
	return r.call(ctx, mesh.AgentRequest{Op: mesh.AgentOpRunRoot, Command: command})
}

func (r *AgentRunner) WriteFileContext(ctx context.Context, path, content, mode string) error {
	if err := validateNodeDeployFile(path, mode); err != nil {
		return err
	}
	_, err := r.call(ctx, mesh.AgentRequest{Op: mesh.AgentOpWriteFile, Path: path, Content: content, Mode: mode})
	return err
}

// A member is reached through its agent, never over SSH, so there is no login
// for a key to open.
func (r *AgentRunner) AppendAuthorizedKeyContext(context.Context, string) error {
	return nil
}

func (r *AgentRunner) Close() error { return nil }
