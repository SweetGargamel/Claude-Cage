package workbench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/sylvinus/agent-vm/internal/paths"
)

// Browser URLs use guest paths. The workbench's project and terminal actions
// use host paths, so an editor's workspace must identify its existing grant.
// Never infer a host drive or a new grant from an arbitrary guest path.
func (s *Server) browserStateForHost(body []byte) ([]byte, error) {
	if !paths.Windows() {
		return body, nil
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, errors.New("浏览器状态无效")
	}
	var tabs []map[string]json.RawMessage
	if err := json.Unmarshal(state["tabs"], &tabs); err != nil {
		return nil, errors.New("浏览器标签状态无效")
	}
	authorized, err := s.projectPaths()
	if err != nil {
		return nil, errors.New("无法读取项目列表")
	}
	byGuest := make(map[string]string, len(authorized))
	for _, hostPath := range authorized {
		byGuest[guestPath(hostPath)] = hostPath
	}
	changed := false
	for _, tab := range tabs {
		var kind, workspace string
		if json.Unmarshal(tab["kind"], &kind) != nil || kind != "editor" || json.Unmarshal(tab["workspace"], &workspace) != nil {
			continue
		}
		if hostPath, ok := byGuest[workspace]; ok && hostPath != workspace {
			tab["workspace"], _ = json.Marshal(hostPath)
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	state["tabs"], err = json.Marshal(tabs)
	if err != nil {
		return nil, err
	}
	return json.Marshal(state)
}

// The guest emits one compact JSON snapshot per data line, plus heartbeat
// comments. Resolve grants for each snapshot so an already-open stream also
// recognizes projects added after it connected.
func (s *Server) writeBrowserEvents(w http.ResponseWriter, body io.Reader) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 16<<10), (1<<20)+16)
	flusher := w.(http.Flusher)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) {
			mapped, err := s.browserStateForHost(line[len("data: "):])
			if err != nil {
				return err
			}
			line = append([]byte("data: "), mapped...)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
		if len(line) == 0 {
			flusher.Flush()
		}
	}
	return scanner.Err()
}
