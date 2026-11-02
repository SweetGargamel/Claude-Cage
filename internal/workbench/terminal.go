package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/vmname"
)

type terminalPTY interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() error
	Kill() error
}

type terminalSession struct {
	mu        sync.Mutex
	id, title string
	workspace string
	cmd       *exec.Cmd
	file      terminalPTY
	history   []byte
	closed    bool
	clients   map[chan []byte]bool
}

func (t *terminalSession) summary() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]any{"id": t.id, "title": t.title, "closed": t.closed, "workspace": t.workspace}
}
func (t *terminalSession) close() {
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		_ = t.file.Kill()
		_ = t.file.Close()
	}
	t.mu.Unlock()
}
func (t *terminalSession) read() {
	b := make([]byte, 16<<10)
	for {
		n, e := t.file.Read(b)
		if n > 0 {
			chunk := append([]byte(nil), b[:n]...)
			t.mu.Lock()
			t.history = append(t.history, chunk...)
			if len(t.history) > 512<<10 {
				t.history = t.history[len(t.history)-(512<<10):]
			}
			for ch := range t.clients {
				select {
				case ch <- chunk:
				default:
					close(ch)
					delete(t.clients, ch)
				}
			}
			t.mu.Unlock()
		}
		if e != nil {
			break
		}
	}
	_ = t.file.Wait()
	t.mu.Lock()
	t.closed = true
	for ch := range t.clients {
		close(ch)
		delete(t.clients, ch)
	}
	t.mu.Unlock()
	_ = t.file.Close()
}
func (s *Server) listTerminals(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, t := range s.terminals {
		out = append(out, t.summary())
	}
	jsonReply(w, out)
}
func (s *Server) newTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.op.TryLock() {
		http.Error(w, "环境正在执行其他操作", 409)
		return
	}
	defer s.op.Unlock()
	s.mu.Lock()
	ready, busy, count := vmAlive(s.profile, s.environmentDir()), s.job.Running, len(s.terminals)
	s.mu.Unlock()
	if !ready || busy {
		http.Error(w, "环境尚未就绪或正在重启", 409)
		return
	}
	if count >= 12 {
		http.Error(w, "最多保留 12 个终端，请先关闭不用的会话", 409)
		return
	}
	var input struct {
		Workspace string `json:"workspace"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	workspace, err := s.projectWorkspace(input.Workspace)
	if err != nil {
		requestError(w, err)
		return
	}
	if err := s.ensureProjectShare(workspace); err != nil {
		requestError(w, err)
		return
	}
	name := vmname.Name(s.environmentDir())
	script := "cd " + shellescape.Quote(guestPath(workspace)) + " && exec env TERM=xterm-256color COLORTERM=truecolor CAGE_WORKBENCH=1 bash -l"
	cmd := s.sshCommand(context.Background(), "-tt", "lima-"+name, "--", script)
	file, err := startTerminal(cmd, 100, 30)
	if err != nil {
		http.Error(w, "无法创建终端连接", 500)
		return
	}
	s.mu.Lock()
	s.terminalSequence++
	number := s.terminalSequence
	s.mu.Unlock()
	t := &terminalSession{workspace: workspace, id: randomToken()[:16], title: fmt.Sprintf("终端 %d", number), cmd: cmd, file: file, clients: map[chan []byte]bool{}}
	s.mu.Lock()
	s.terminals[t.id] = t
	s.mu.Unlock()
	go t.read()
	jsonReply(w, t.summary())
}
func (s *Server) closeTerminal(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t := s.terminals[r.PathValue("id")]
	delete(s.terminals, r.PathValue("id"))
	s.mu.Unlock()
	if t != nil {
		t.close()
	}
	jsonReply(w, map[string]bool{"ok": true})
}
func (s *Server) upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == s.origin }, ReadBufferSize: 16384, WriteBufferSize: 16384}
	return up.Upgrade(w, r, nil)
}
func (s *Server) terminalStream(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	t := s.terminals[r.PathValue("id")]
	s.mu.Unlock()
	if t == nil {
		http.NotFound(w, r)
		return
	}
	ws, e := s.upgrade(w, r)
	if e != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)
	ch := make(chan []byte, 64)
	t.mu.Lock()
	history := append([]byte(nil), t.history...)
	closed := t.closed
	if !closed {
		t.clients[ch] = true
	}
	t.mu.Unlock()
	defer func() { t.mu.Lock(); delete(t.clients, ch); t.mu.Unlock() }()
	if len(history) > 0 {
		if e = ws.WriteMessage(websocket.BinaryMessage, history); e != nil {
			return
		}
	}
	if closed {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for chunk := range ch {
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if ws.WriteMessage(websocket.BinaryMessage, chunk) != nil {
				break
			}
		}
		_ = ws.Close()
	}()
	for {
		_, data, e := ws.ReadMessage()
		if e != nil {
			break
		}
		var v struct {
			Type string `json:"type"`
			Data string `json:"data"`
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		if json.Unmarshal(data, &v) != nil {
			continue
		}
		switch v.Type {
		case "input":
			_, _ = io.WriteString(t.file, v.Data)
		case "resize":
			if v.Cols > 1 && v.Cols < 600 && v.Rows > 1 && v.Rows < 400 {
				_ = t.file.Resize(v.Cols, v.Rows)
			}
		}
	}
	t.mu.Lock()
	if t.clients[ch] {
		delete(t.clients, ch)
		close(ch)
	}
	t.mu.Unlock()
	<-done
}
func (s *Server) vnc(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		http.Error(w, "环境尚未就绪", 409)
		return
	}
	remote, e := net.DialTimeout("tcp", "127.0.0.1:15900", 3*time.Second)
	if e != nil {
		http.Error(w, "画面连接暂时不可用", 502)
		return
	}
	defer remote.Close()
	ws, e := s.upgrade(w, r)
	if e != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(4 << 20)
	go func() {
		defer ws.Close()
		b := make([]byte, 64<<10)
		for {
			n, e := remote.Read(b)
			if n > 0 {
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteMessage(websocket.BinaryMessage, b[:n]) != nil {
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	for {
		kind, b, e := ws.ReadMessage()
		if e != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			if _, e = remote.Write(b); e != nil {
				return
			}
		}
	}
}
