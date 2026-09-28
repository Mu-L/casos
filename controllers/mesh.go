package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/beego/beego/logs"
	"github.com/casosorg/casos/deploy"
	"github.com/casosorg/casos/mesh"
	"github.com/casosorg/casos/object"
	"github.com/casosorg/casos/util"
)

var meshConfig atomic.Pointer[mesh.Config]

func SetMeshConfig(cfg mesh.Config) {
	meshConfig.Store(&cfg)
}

func getMeshConfig() mesh.Config {
	if cfg := meshConfig.Load(); cfg != nil {
		return *cfg
	}
	return mesh.Config{Role: mesh.RoleStandalone}
}

type meshStatus struct {
	Role          mesh.Role `json:"role"`
	PublicURL     string    `json:"publicUrl,omitempty"`
	OverlayIP     string    `json:"overlayIp,omitempty"`
	HubURL        string    `json:"hubUrl,omitempty"`
	Machine       string    `json:"machine,omitempty"`
	JoinedTime    string    `json:"joinedTime,omitempty"`
	Connected     bool      `json:"connected"`
	ConsoleURL    string    `json:"consoleUrl,omitempty"`
	OnlineMembers []string  `json:"onlineMembers,omitempty"`
}

// GetMeshStatus says which part this CasOS plays in a multi-machine cloud.
// @router /api/get-mesh-status [get]
func (c *ApiController) GetMeshStatus() {
	if c.RequireAdmin() {
		return
	}
	cfg := getMeshConfig()
	status := meshStatus{Role: cfg.Role}
	switch cfg.Role {
	case mesh.RoleHub:
		status.PublicURL = cfg.PublicURL()
		if hub := mesh.CurrentHub(); hub != nil {
			status.OverlayIP = hub.OverlayIP().String()
		}
		members, err := object.GetMeshMembers(meshMemberOwner)
		if err != nil {
			c.ResponseError(err.Error())
			return
		}
		for _, member := range members {
			if mesh.Agent(member.Owner+"/"+member.Name) != nil {
				status.OnlineMembers = append(status.OnlineMembers, member.Name)
			}
		}
	case mesh.RoleMember:
		m, err := mesh.LoadMembership(cfg.DataDir)
		if err != nil {
			c.ResponseError(err.Error())
			return
		}
		if m != nil {
			status.HubURL = m.HubURL
			status.Machine = m.Machine
			status.JoinedTime = m.JoinedTime
			status.ConsoleURL = m.ConsoleURL
		}
		status.Connected = mesh.MemberConnected()
	}
	c.ResponseOk(status)
}

const meshMemberOwner = "admin"

type addMeshInviteRequest struct {
	Hours int `json:"hours"`
	Uses  int `json:"uses"`
}

type addMeshInviteResult struct {
	*object.MeshInvite
	Token   string `json:"token"`
	Command string `json:"command"`
	HubURL  string `json:"hubUrl"`
}

// AddMeshInvite answers with the invite token, the only time it is readable.
// @router /api/add-mesh-invite [post]
func (c *ApiController) AddMeshInvite() {
	if c.RequireAdmin() {
		return
	}
	hub := mesh.CurrentHub()
	if hub == nil {
		c.ResponseError("this CasOS is not a cloud hub: turn the hub on first")
		return
	}
	var req addMeshInviteRequest
	if err := json.Unmarshal(c.Ctx.Input.RequestBody, &req); err != nil {
		c.ResponseError("invalid request body: " + err.Error())
		return
	}
	if req.Hours <= 0 {
		req.Hours = 24
	}
	if req.Uses <= 0 {
		req.Uses = 1
	}
	invite, secret, err := object.AddMeshInvite(meshMemberOwner, time.Duration(req.Hours)*time.Hour, req.Uses)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}
	token := hub.InviteToken(invite.Name, secret)
	hubURL := hub.Config().PublicURL()
	c.ResponseOk(addMeshInviteResult{
		MeshInvite: invite,
		Token:      token,
		HubURL:     hubURL,
		Command:    fmt.Sprintf("casos join %s %s", hubURL, token),
	})
}

// @router /api/get-mesh-invites [get]
func (c *ApiController) GetMeshInvites() {
	if c.RequireAdmin() {
		return
	}
	invites, err := object.GetMeshInvites(meshMemberOwner)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}
	c.ResponseOk(invites)
}

// @router /api/delete-mesh-invite [post]
func (c *ApiController) DeleteMeshInvite() {
	if c.RequireAdmin() {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(c.Ctx.Input.RequestBody, &req); err != nil {
		c.ResponseError("invalid request body: " + err.Error())
		return
	}
	ok, err := object.DeleteMeshInvite(meshMemberOwner, req.Name)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}
	c.ResponseOk(ok)
}

// EnableMeshHub turns this standalone CasOS into a cloud hub and restarts it.
// @router /api/enable-mesh-hub [post]
func (c *ApiController) EnableMeshHub() {
	if c.RequireAdmin() {
		return
	}
	cfg := getMeshConfig()
	if cfg.Role != mesh.RoleStandalone {
		c.ResponseError(fmt.Sprintf("this CasOS is already a cloud %s", cfg.Role))
		return
	}
	var req struct {
		PublicAddress string `json:"publicAddress"`
	}
	if err := json.Unmarshal(c.Ctx.Input.RequestBody, &req); err != nil {
		c.ResponseError("invalid request body: " + err.Error())
		return
	}
	if err := mesh.EnableHub(cfg, req.PublicAddress); err != nil {
		c.ResponseError(err.Error())
		return
	}
	c.ResponseOk()
	restartSoon("to become a cloud hub")
}

// JoinMesh makes this standalone CasOS a member of another CasOS's cloud and
// restarts it as one.
// @router /api/join-mesh [post]
func (c *ApiController) JoinMesh() {
	if c.RequireAdmin() {
		return
	}
	cfg := getMeshConfig()
	if cfg.Role != mesh.RoleStandalone {
		c.ResponseError(fmt.Sprintf("this CasOS is already a cloud %s", cfg.Role))
		return
	}
	var req struct {
		HubURL string `json:"hubUrl"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(c.Ctx.Input.RequestBody, &req); err != nil {
		c.ResponseError("invalid request body: " + err.Error())
		return
	}
	hostname, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(c.Ctx.Request.Context(), time.Minute)
	defer cancel()
	m, err := mesh.Join(ctx, cfg, req.HubURL, req.Token, hostname, runtime.GOOS)
	if err != nil {
		c.ResponseError(err.Error())
		return
	}
	c.ResponseOk(m.Machine)
	restartSoon("to join the cloud at " + m.HubURL)
}

// LeaveMesh takes this member out of its cloud and restarts it standalone.
// @router /api/leave-mesh [post]
func (c *ApiController) LeaveMesh() {
	if c.RequireAdmin() {
		return
	}
	cfg := getMeshConfig()
	if cfg.Role != mesh.RoleMember {
		c.ResponseError("this CasOS has not joined a cloud")
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.Unmarshal(c.Ctx.Input.RequestBody, &req)
	m, err := mesh.LoadMembership(cfg.DataDir)
	if err != nil || m == nil {
		c.ResponseError("this CasOS has not joined a cloud")
		return
	}
	ctx, cancel := context.WithTimeout(c.Ctx.Request.Context(), 2*time.Minute)
	defer cancel()
	if err := deploy.LeaveMeshCloud(ctx, cfg, m, req.Force); err != nil {
		c.ResponseError(err.Error())
		return
	}
	c.ResponseOk()
	restartSoon("to run on its own again")
}

func restartSoon(reason string) {
	go func() {
		// Lets the response that triggered this reach the browser first.
		time.Sleep(time.Second)
		logs.Info("restarting %s", reason)
		if err := util.RestartSelf(); err != nil {
			logs.Error("restart: %v; start CasOS again by hand", err)
		}
		os.Exit(0)
	}()
}
