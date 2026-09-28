package mesh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// A member machine keeps one WebSocket open to its hub, and the hub drives
// the node deployment through it: the hub sends a request, the member runs it
// on its node and answers. The member dials out, so it needs neither a public
// address nor an SSH server.

const (
	AgentOpRun       = "run"
	AgentOpRunRoot   = "runRoot"
	AgentOpWriteFile = "writeFile"

	agentPingPeriod = 30 * time.Second
	agentReadWait   = 90 * time.Second
	agentWriteWait  = 30 * time.Second
)

type AgentRequest struct {
	ID      uint64 `json:"id"`
	Op      string `json:"op"`
	Command string `json:"command,omitempty"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	Mode    string `json:"mode,omitempty"`
	// Timeout bounds the request on the member, so a command the hub gave up
	// on does not keep running there.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

type AgentResponse struct {
	ID     uint64 `json:"id"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

var ErrAgentOffline = errors.New("the machine is not connected to this hub: check that CasOS is running on it")

// AgentSession is the hub's end of one member's connection.
type AgentSession struct {
	id      string
	conn    *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[uint64]chan AgentResponse
	done    chan struct{}
}

var agentSessions sync.Map

// Agent returns the live session of the member machine id, or nil.
func Agent(id string) *AgentSession {
	if s, ok := agentSessions.Load(id); ok {
		return s.(*AgentSession)
	}
	return nil
}

var agentUpgrader = websocket.Upgrader{
	ReadBufferSize:  64 << 10,
	WriteBufferSize: 64 << 10,
}

// AcceptAgent upgrades an authenticated request into the session of member
// id, replacing any older session of the same member. It returns once the
// connection is established; Done reports when it ends.
func AcceptAgent(w http.ResponseWriter, r *http.Request, id string) (*AgentSession, error) {
	conn, err := agentUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, err
	}
	s := &AgentSession{id: id, conn: conn, pending: map[uint64]chan AgentResponse{}, done: make(chan struct{})}
	if old, loaded := agentSessions.Swap(id, s); loaded {
		old.(*AgentSession).close()
	}
	go s.readLoop()
	go s.pingLoop()
	return s, nil
}

func (s *AgentSession) Done() <-chan struct{} { return s.done }

func (s *AgentSession) readLoop() {
	defer s.close()
	_ = s.conn.SetReadDeadline(time.Now().Add(agentReadWait))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(agentReadWait))
	})
	for {
		var resp AgentResponse
		if err := s.conn.ReadJSON(&resp); err != nil {
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(agentReadWait))
		s.mu.Lock()
		ch := s.pending[resp.ID]
		delete(s.pending, resp.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- resp
		}
	}
}

func (s *AgentSession) pingLoop() {
	ticker := time.NewTicker(agentPingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.writeMu.Lock()
			err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(agentWriteWait))
			s.writeMu.Unlock()
			if err != nil {
				s.close()
				return
			}
		}
	}
}

func (s *AgentSession) close() {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return
	default:
	}
	close(s.done)
	s.mu.Unlock()
	_ = s.conn.Close()
	agentSessions.CompareAndDelete(s.id, s)
}

// Call sends req to the member and waits for its answer.
func (s *AgentSession) Call(ctx context.Context, req AgentRequest) (AgentResponse, error) {
	ch := make(chan AgentResponse, 1)
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return AgentResponse{}, ErrAgentOffline
	default:
	}
	s.nextID++
	req.ID = s.nextID
	s.pending[req.ID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, req.ID)
		s.mu.Unlock()
	}()

	if deadline, ok := ctx.Deadline(); ok {
		req.TimeoutSeconds = int(time.Until(deadline).Seconds()) + 1
	}
	s.writeMu.Lock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(agentWriteWait))
	err := s.conn.WriteJSON(req)
	s.writeMu.Unlock()
	if err != nil {
		s.close()
		return AgentResponse{}, fmt.Errorf("send to %s: %w", s.id, err)
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-s.done:
		return AgentResponse{}, fmt.Errorf("the connection to %s dropped while it was working: %w", s.id, ErrAgentOffline)
	case <-ctx.Done():
		return AgentResponse{}, ctx.Err()
	}
}
