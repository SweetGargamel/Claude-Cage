package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/lima-vm/sshocker/pkg/reversesshfs"
	"github.com/sylvinus/agent-vm/internal/mounts"
	"github.com/sylvinus/agent-vm/internal/vmname"
)

type projectTransport struct {
	io.ReadCloser
	io.WriteCloser
}

func (p projectTransport) Close() error { _ = p.ReadCloser.Close(); return p.WriteCloser.Close() }

type projectShare struct {
	cmd       *exec.Cmd
	transport projectTransport
	done      chan struct{}
	once      sync.Once
}

func (p *projectShare) close() {
	p.once.Do(func() {
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		if p.transport.ReadCloser != nil {
			_ = p.transport.Close()
		}
	})
}

// Called under the lifecycle lock. Each SFTP server is rooted in exactly the
// directory the user opened; switching projects keeps existing mounts alive.
func (s *Server) ensureProjectShare(path string) error {
	path, err := validateWorkspace(path, s.profile)
	if err != nil {
		return err
	}
	if path == s.environmentDir() {
		return nil
	}
	s.mu.Lock()
	current := s.projectShares[path]
	s.mu.Unlock()
	if current != nil {
		select {
		case <-current.done:
			current.close()
		default:
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(s.operationContext(), 15*time.Second)
	defer cancel()
	quoted := shellescape.Quote(guestPath(path))
	// Stale FUSE mounts can remain after a control-service restart. Only reconnect
	// this explicitly authorized path, without changing other projects' mounts.
	if _, err = s.guest(ctx, "if mountpoint -q "+quoted+"; then fusermount3 -uz "+quoted+"; fi; sudo mkdir -p "+quoted+" && sudo chown \"$(id -u):$(id -g)\" "+quoted); err != nil {
		return errors.New("无法在 Cage 中准备项目目录")
	}
	probe, err := os.CreateTemp(path, ".cage-mount-probe-*")
	if err != nil {
		return fmt.Errorf("无法验证项目目录的读写权限：%w", err)
	}
	defer os.Remove(probe.Name())
	challenge := randomToken()
	if _, err = probe.WriteString(challenge); err != nil {
		_ = probe.Close()
		return err
	}
	if err = probe.Close(); err != nil {
		return err
	}
	script := "exec sshfs " + shellescape.Quote(":"+filepath.ToSlash(path)) + " " + quoted + " -o slave,allow_other,cache=no,nodev,nosuid"
	cmd := s.sshCommand(context.Background(), "-T", "lima-"+vmname.Name(s.environmentDir()), "--", script)
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	share := &projectShare{cmd: cmd, transport: projectTransport{output, input}, done: make(chan struct{})}
	if err = cmd.Start(); err != nil {
		_ = share.transport.Close()
		return err
	}
	go func() {
		_ = reversesshfs.ServeConfined(share.transport, path, false, mounts.BaseNames)
		_ = share.transport.Close()
		_ = cmd.Wait()
		close(share.done)
	}()
	for {
		if _, err = s.guest(ctx, "mountpoint -q "+quoted+" && test \"$(cat "+shellescape.Quote(guestPath(probe.Name()))+")\" = "+shellescape.Quote(challenge)); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			share.close()
			return errors.New("项目文件夹连接超时，原有环境与会话保持运行")
		case <-share.done:
			share.close()
			return errors.New("项目文件夹未能连接，原有环境与会话保持运行")
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.mu.Lock()
	if s.projectShares == nil {
		s.projectShares = map[string]*projectShare{}
	}
	s.projectShares[path] = share
	s.mu.Unlock()
	return nil
}

type sharedProjects struct {
	Paths []string `json:"paths"`
}

// The saved directory grants are the project list. Include the default and
// current directories for profiles created before projects.json existed.
func (s *Server) projectPaths() ([]string, error) {
	var saved sharedProjects
	if b, err := os.ReadFile(filepath.Join(s.profile, "projects.json")); err == nil {
		if err = json.Unmarshal(b, &saved); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	paths := []string{s.environmentDir()}
	for _, path := range append(saved.Paths, s.workspace()) {
		found := path == ""
		for _, existing := range paths {
			found = found || path == existing
		}
		if !found {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func (s *Server) projectWorkspace(path string) (string, error) {
	if path == "" {
		path = s.workspace()
	}
	paths, err := s.projectPaths()
	if err != nil {
		return "", errors.New("无法读取项目列表，请检查项目配置")
	}
	for _, authorized := range paths {
		if path == authorized {
			return validateWorkspace(path, s.profile)
		}
	}
	return "", errors.New("请先通过打开项目添加该文件夹")
}

func (s *Server) rememberSharedProject(path string) error {
	if path == s.environmentDir() {
		return nil
	}
	var saved sharedProjects
	if b, err := os.ReadFile(filepath.Join(s.profile, "projects.json")); err == nil {
		if err = json.Unmarshal(b, &saved); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, p := range saved.Paths {
		if p == path {
			return nil
		}
	}
	saved.Paths = append(saved.Paths, path)
	return writePrivate(filepath.Join(s.profile, "projects.json"), saved)
}
func (s *Server) restoreProjectShares() {
	var saved sharedProjects
	if b, err := os.ReadFile(filepath.Join(s.profile, "projects.json")); err == nil {
		_ = json.Unmarshal(b, &saved)
	}
	current := s.workspace()
	if current != "" && current != s.environmentDir() {
		found := false
		for _, p := range saved.Paths {
			found = found || p == current
		}
		if !found {
			saved.Paths = append(saved.Paths, current)
		}
	}
	var failures []error
	for _, path := range saved.Paths {
		if err := s.ensureProjectShare(path); err != nil {
			failures = append(failures, err)
		} else if err := s.rememberSharedProject(path); err != nil {
			failures = append(failures, err)
		}
	}
	s.mu.Lock()
	s.projectError = ""
	if len(failures) > 0 {
		s.projectError = "部分项目文件夹未能恢复连接，请重新打开对应项目。环境和浏览器可继续使用。"
	}
	s.mu.Unlock()
}
func (s *Server) closeProjectShares() {
	s.mu.Lock()
	shares := s.projectShares
	s.projectShares = nil
	s.mu.Unlock()
	for _, share := range shares {
		share.close()
	}
}
