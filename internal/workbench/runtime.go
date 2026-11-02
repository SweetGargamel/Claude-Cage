package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"github.com/sylvinus/agent-vm/internal/vmname"
)

var loadConfig = cage.Load
var defaultConfig = cage.DefaultConfig

type guestCredentials struct {
	Token       string `json:"token"`
	VNCPassword string `json:"-"`
}

func (s *Server) workspace() string      { s.mu.Lock(); defer s.mu.Unlock(); return s.preferences.Workspace }
func (s *Server) environmentDir() string { return environmentWorkspace(s.profile) }
func (s *Server) sshCommand(ctx context.Context, args ...string) *exec.Cmd {
	name := vmname.Name(s.environmentDir())
	cfg := filepath.Join(s.profile, "state", "lima", name, "ssh.config")
	base := []string{"-q", "-e", "none", "-F", cfg, "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "PermitLocalCommand=no", "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no", "-o", "ProxyCommand=none", "-o", "ProxyJump=none", "-o", "ConnectTimeout=3", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "-o", "ForwardAgent=no", "-o", "IdentityAgent=none", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes", "-o", "ControlPath=none"}
	base = append(base, args...)
	cmd := exec.CommandContext(ctx, sshExecutable(), base...)
	cmd.Env = sshEnvironment()
	hideCommand(cmd)
	return cmd
}
func (s *Server) guest(ctx context.Context, script string) ([]byte, error) {
	name := vmname.Name(s.environmentDir())
	cmd := s.sshCommand(ctx, "-T", "lima-"+name, "--", script)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, (64<<10)+1))
	if readErr != nil || len(data) > 64<<10 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.New("guest control response exceeded its limit")
	}
	if err = cmd.Wait(); err != nil {
		return nil, err
	}
	return data, nil
}
func (s *Server) attach() error {

	if !s.vmRunning() {
		return errors.New("环境尚未启动")
	}
	ctx, cancel := controlContext()
	defer cancel()
	b, err := s.guest(ctx, guestConnectionSnapshot)
	if err != nil {
		return errors.New("环境尚未准备好")
	}
	credentials, manifest, err := parseGuestConnection(b)
	s.mu.Lock()
	s.guestManifest = manifest
	s.guestUpdateRequired = errors.Is(err, errGuestUpdateRequired)
	s.guestUpdateError = ""
	if err != nil {
		s.ready = false
		s.guestUpdateError = err.Error()
	}
	s.mu.Unlock()
	if err != nil {
		if errors.Is(err, errGuestUpdateRequired) {
			_ = s.closeProxyGate("环境组件需要更新，外网保持关闭")
		}
		return err
	}
	if _, err = s.guestAPI(ctx, credentials.Token, "GET", "/health", nil); err != nil {
		return err
	}
	s.mu.Lock()
	home := browserHomeURL(s.preferences)
	s.mu.Unlock()
	if err = s.setBrowserHome(ctx, credentials.Token, home); err != nil {
		return err
	}
	s.mu.Lock()
	s.credentials = credentials
	s.ready = true
	s.mu.Unlock()
	s.restoreProjectShares()
	return nil
}
func (s *Server) monitor() {
	for {
		if s.op.TryLock() {
			if !s.busy() {
				s.mu.Lock()
				ready := s.ready
				token := s.credentials.Token
				s.mu.Unlock()
				if !ready {
					_ = s.attach()
				} else {
					ctx, cancel := controlContext()
					_, err := s.guestAPI(ctx, token, "GET", "/health", nil)
					cancel()
					if err != nil {
						s.mu.Lock()
						s.ready = false
						s.mu.Unlock()
					}
				}
			}
			s.op.Unlock()
		}
		time.Sleep(5 * time.Second)
	}
}

type jobOutput struct{ s *Server }

func (w jobOutput) Write(b []byte) (int, error) {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	if w.s.activeJobID != "" && w.s.activeJobID != w.s.job.ID {
		return len(b), nil
	}
	w.s.job.Log += string(b)
	if len(w.s.job.Log) > 24000 {
		w.s.job.Log = w.s.job.Log[len(w.s.job.Log)-24000:]
	}
	return len(b), nil
}
func (s *Server) runCage(args ...string) error {
	ctx, cancel := context.WithTimeout(s.operationContext(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.exe, args...)
	hideCommand(cmd)
	cmd.Dir = s.environmentDir()
	if cmd.Dir == "" {
		cmd.Dir = s.profile
	}
	cmd.Env = append(os.Environ(), cage.ConfigEnv+"="+s.configPath)
	s.mu.Lock()
	jobID := s.activeJobID
	s.mu.Unlock()
	cmd.Env = append(cmd.Env, cage.JobEnv+"="+jobID)
	cmd.Stdout = jobOutput{s}
	cmd.Stderr = jobOutput{s}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s 未完成，请查看操作详情并重试", args[0])
	}
	return nil
}
func (s *Server) startEnvironment(syncTimezone bool) error {
	// Check before proxy activation or the first image download. The UI cannot
	// bypass this gate by calling /start or applying settings directly.
	if err := requireHostGit(s.operationContext()); err != nil {
		return err
	}
	if _, err := validateWorkspace(s.environmentDir(), s.profile); err != nil {
		return err
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	if err = cfg.RequireManagedProxy(s.profile); err != nil {
		_ = s.closeProxyGate(err.Error())
		return err
	}
	{
		p, e := managedproxy.Load(s.profile)
		if e != nil {
			return e
		}
		if p.Selected == "" || p.Endpoint() != cfg.ProxyURL || p.Revision != cfg.ProxyRevision {
			return errors.New("内置代理配置未应用，请重新选择节点")
		}
		name, e := physicalProxyInterface(s.operationContext())
		if e != nil {
			_ = s.closeProxyGate(e.Error())
			return e
		}
		bound, _ := managedproxy.BoundInterface(filepath.Join(managedproxy.Dir(s.profile), "config.yaml"))
		if bound != name || !managedproxy.Running(s.operationContext(), s.profile, p) {
			// Every core start changes the report generation, including recovery.
			if e = s.activateProxy(p, cfg); e != nil {
				return e
			}
			cfg, e = loadConfig(s.configPath)
			if e != nil {
				return e
			}
		}
	}

	attachErr := s.attach()
	if errors.Is(attachErr, errGuestUpdateRequired) {
		return s.synchronizeEnvironment()
	}
	if attachErr == nil {
		client, gateErr := s.gateClient()
		if gateErr == nil {
			state, e := client.Status(context.Background())
			if e == nil {
				if state.Mode == cage.GateOpen && state.Report != nil && state.Report.Authorizes(state.RunID, cfg.Fingerprint(), time.Now()) == nil {
					s.publishGate(state)
					return nil
				}
				return s.runSafetyCheck()
			}
		}
		s.setMessage("正在重启环境以加载 VM 外网络门槛…")
		if err := s.stopEnvironment(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	preferences := s.preferences
	auto := preferences.AutoTimezone || preferences.AutoLanguage
	s.mu.Unlock()
	s.setMessage("正在验证代理出口…")
	loc, probeErr := probeEnvironmentLocation(s.operationContext(), cfg, preferences)
	if s.operationContext().Err() != nil {
		return s.operationContext().Err()
	}
	s.mu.Lock()
	if probeErr != nil {
		s.networkError = probeErr.Error()
	} else {
		s.location = &loc
		s.networkError = ""
	}
	s.mu.Unlock()
	if cfg.ExitPolicy.NeedsBinding() && loc.IP != "" && !s.vmRunning() {
		bound := cfg.ExitPolicy.BindMissing(loc.exitLocation())
		if bound != cfg.ExitPolicy {
			cfg.ExitPolicy = bound
			if err = writePrivate(s.configPath, cfg); err != nil {
				return err
			}
		}
	}
	if auto && syncTimezone && !s.vmRunning() {
		if probeErr != nil {
			if _, e := os.Stat(filepath.Join(s.profile, "state", ".agent-vm-base-version")); e != nil {
				return probeErr
			}
		}
		if probeErr == nil {
			observed := loc.exitLocation()
			within, _ := cfg.ExitPolicy.Evaluate(loc.IP, &observed)
			updated := cfg
			if within == cage.CheckPassed {
				updated = autoRegionSettings(cfg, preferences, loc)
			}
			if updated != cfg {
				// Changing a live hostagent's profile is forbidden. Stop before replacing it.
				s.setMessage("正在应用出口所在地的时区与语言建议…")
				if err = s.stopEnvironment(); err != nil {
					return err
				}
				cfg = updated
				if err = writePrivate(s.configPath, cfg); err != nil {
					return err
				}
			}
		}
	} else if probeErr != nil {
		s.setMessage("正在检查代理连通性…")
		if _, e := os.Stat(filepath.Join(s.profile, "state", ".agent-vm-base-version")); e != nil {
			if err = s.runCage("check"); err != nil {
				return err
			}
		}
	}
	if _, err = os.Stat(filepath.Join(s.profile, "state", ".agent-vm-base-version")); os.IsNotExist(err) {
		s.setMessage("首次准备 Linux 环境与开发工具，可能需要数分钟…")
		if err = s.runCage("setup"); err != nil {
			return err
		}
	}
	s.setMessage("正在启动浏览器、编辑器与共享文件…")
	if err = s.runCage("up"); err != nil {
		return err
	}
	for i := 0; i < 8; i++ {
		if s.operationContext().Err() != nil {
			return s.operationContext().Err()
		}
		if err = s.attach(); err == nil {
			return s.runSafetyCheck()
		}
		time.Sleep(time.Second)
	}
	return errors.New("环境已启动，但浏览器连接尚未就绪。请重试连接或查看操作详情。")
}
func (s *Server) stopEnvironment() (returned error) {
	s.setMessage("正在停止 Cage 环境…")
	s.mu.Lock()
	explicitStop := s.job.Kind == "stop"
	s.mu.Unlock()
	defer func() {
		if explicitStop && returned == nil {
			returned = managedproxy.Stop(s.profile)
		}
	}()
	if explicitStop && cage.InstanceJobID(filepath.Join(s.profile, "state"), "base-building") != "" {
		if err := s.runCage("stop", "base-building"); err != nil {
			return err
		}
	}
	if client, err := s.gateClient(); err == nil {
		if state, e := client.Close(context.Background(), "环境正在停止"); e == nil {
			s.publishGate(state)
		}
	}
	s.mu.Lock()
	sessions := make([]*terminalSession, 0, len(s.terminals))
	for _, t := range s.terminals {
		sessions = append(sessions, t)
	}
	s.ready = false
	s.credentials = guestCredentials{}
	s.guestManifest = nil
	s.guestUpdateRequired = false
	s.guestUpdateError = ""
	s.safety.Error = ""
	s.mu.Unlock()
	for _, t := range sessions {
		t.close()
	}
	s.closeProjectShares()
	if s.workspace() == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.profile, "state", "lima", vmname.Name(s.environmentDir()), "lima.yaml")); os.IsNotExist(err) {
		return nil
	}
	if err := s.runCage("stop"); err != nil {
		return err
	}
	return nil
}
func (s *Server) apply(v savedProfile, validate bool) error {
	old, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	// Network endpoints are generated by the node manager, including restore.
	v.Config.ProxyURL, v.Config.ProxyRevision = old.ProxyURL, old.ProxyRevision
	s.mu.Lock()
	oldPrefs := s.preferences
	resumeVersion := s.safety.pauseVersion
	if s.activeJobID != "" {
		resumeVersion = s.activePauseVersion
	}
	wasReady := vmAlive(s.profile, s.environmentDir())
	s.mu.Unlock()
	changed := old != v.Config || oldPrefs.AutoTimezone != v.Preferences.AutoTimezone || oldPrefs.AutoLanguage != v.Preferences.AutoLanguage
	if changed && validate && old.RequireManagedProxy(s.profile) == nil {
		s.setMessage("正在验证新出口，现有环境保持运行…")
		loc, e := probeEnvironmentLocation(s.operationContext(), v.Config, v.Preferences)
		if e != nil {
			if v.Preferences.AutoTimezone || v.Preferences.AutoLanguage || loc.IP == "" {
				return e
			}
			s.mu.Lock()
			s.networkError = e.Error()
			s.mu.Unlock()
		} else {
			v.Config = autoRegionSettings(v.Config, v.Preferences, loc)
			s.mu.Lock()
			s.location = &loc
			s.networkError = ""
			s.mu.Unlock()
		}
		v.Config.ExitPolicy = v.Config.ExitPolicy.BindMissing(loc.exitLocation())
		observed := loc.exitLocation()
		if result, message := v.Config.ExitPolicy.Evaluate(loc.IP, &observed); result != cage.CheckPassed {
			if result == cage.CheckUnknown && e != nil && !v.Preferences.AutoTimezone && !v.Preferences.AutoLanguage {
				return errors.New("时区和语言已选择手动设置，但出口规则仍要求确认国家或城市。请重试位置检测，或在“IP 变化规则”中明确选择静态 IP 或不限制地区；原配置保持不变")
			}
			return errors.New(message)
		}
	}
	if changed {
		if err := s.resumeNetworkIntentAt(resumeVersion); err != nil {
			return err
		}
		if s.operationContext().Err() != nil {
			return s.operationContext().Err()
		}
		if err = writePrivate(filepath.Join(s.profile, "previous-profile.json"), savedProfile{old, oldPrefs}); err != nil {
			return err
		}
		if err = s.stopEnvironment(); err != nil {
			return err
		}
	}
	// A project is a live directory grant in the same VM, never a lifecycle change.
	if v.Preferences.Workspace != "" && s.vmRunning() {
		s.setMessage("正在连接项目文件夹…")
		if err = s.ensureProjectShare(v.Preferences.Workspace); err != nil {
			return err
		}
		if err = s.rememberSharedProject(v.Preferences.Workspace); err != nil {
			return err
		}
	}
	if err = writePrivate(s.configPath, v.Config); err != nil {
		return err
	}
	if err = writePrivate(filepath.Join(s.profile, "workbench.json"), v.Preferences); err != nil {
		_ = writePrivate(s.configPath, old)
		return err
	}
	s.mu.Lock()
	s.preferences = v.Preferences
	token, ready := s.credentials.Token, s.ready
	s.mu.Unlock()
	if !changed && ready && browserHomeURL(oldPrefs) != browserHomeURL(v.Preferences) {
		ctx, cancel := controlContext()
		defer cancel()
		if err = s.setBrowserHome(ctx, token, browserHomeURL(v.Preferences)); err != nil {
			return fmt.Errorf("设置已保存，但浏览器暂时未能应用默认网站，请恢复连接后重试：%w", err)
		}
	}
	if changed && wasReady {
		if err = s.startEnvironment(false); err != nil {
			return fmt.Errorf("新设置已保存，但环境未能启动。可以重试启动，或恢复上一份配置：%w", err)
		}
	}
	return nil
}

func (s *Server) setBrowserHome(ctx context.Context, token, home string) error {
	body, _ := json.Marshal(map[string]string{"url": home})
	_, err := s.guestAPI(ctx, token, "POST", "/homepage", body)
	return err
}
func (s *Server) guestAPI(ctx context.Context, token, method, path string, body []byte) ([]byte, error) {
	s.mu.Lock()
	if s.guestClient == nil {
		s.guestClient = &http.Client{Transport: &http.Transport{Proxy: nil, IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 4, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	}
	client := s.guestClient
	s.mu.Unlock()
	if path == "/audit" {
		auditClient := *client
		auditClient.Timeout = 20 * time.Second
		client = &auditClient
	}
	if path == "/editor-restart" {
		recoveryClient := *client
		recoveryClient.Timeout = 30 * time.Second
		client = &recoveryClient
	}
	req, _ := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:18500"+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("浏览器服务暂时无法连接")
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, errors.New("浏览器响应中断")
	}
	if res.StatusCode != 200 {
		return nil, errors.New("浏览器操作失败，请刷新标签后重试")
	}
	return b, nil
}
func (s *Server) browser(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if r.Method == "GET" {
		action = "tabs"
	}
	allowed := map[string]bool{"tabs": true, "new": true, "activate": true, "close": true, "navigate": true, "back": true, "forward": true, "reload": true, "editor": true, "editor-restart": true, "download-retry": true, "resize": true, "insert-text": true}
	if !allowed[action] {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		http.Error(w, "环境尚未就绪", 409)
		return
	}
	var body []byte
	if r.Method != "GET" {
		var input struct {
			ID        string `json:"id"`
			URL       string `json:"url"`
			Width     int    `json:"width,omitempty"`
			Height    int    `json:"height,omitempty"`
			Workspace string `json:"workspace,omitempty"`
			Text      string `json:"text,omitempty"`
		}
		if !readJSON(w, r, &input) {
			return
		}
		if action == "insert-text" && (input.ID == "" || input.Text == "" || len(input.Text) > 8192) {
			http.Error(w, "输入文字或目标标签无效", http.StatusBadRequest)
			return
		}
		if action == "new" && input.URL == "" {
			s.mu.Lock()
			input.URL = browserHomeURL(s.preferences)
			s.mu.Unlock()
		}
		if action == "editor" || action == "editor-restart" {
			if !s.op.TryLock() {
				http.Error(w, "环境正在执行其他操作", 409)
				return
			}
			defer s.op.Unlock()
			workspace, err := s.projectWorkspace(input.Workspace)
			if err != nil {
				requestError(w, err)
				return
			}
			input.Workspace = workspace
			if err := s.ensureProjectShare(input.Workspace); err != nil {
				requestError(w, err)
				return
			}
			input.Workspace = guestPath(workspace)
		}
		body, _ = json.Marshal(input)
	}
	b, err := s.guestAPI(r.Context(), token, r.Method, "/"+action, body)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	if action == "tabs" {
		b, err = s.browserStateForHost(b)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// Interface recovery uses the ordinary cancellable job queue. Location changes
// remain governed by the saved exit policy and never replace its baseline.
func (s *Server) monitorNetwork() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if s.op.TryLock() {
			s.monitorProxyInterface()
			s.checkProxyHealth()
			s.monitorSafety()
			s.refreshProxySubscriptions()
			s.op.Unlock()
		}
		<-ticker.C
	}
}

func (s *Server) browserEvents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		http.Error(w, "Browser unavailable", 409)
		return
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	req, _ := http.NewRequestWithContext(r.Context(), "GET", "http://127.0.0.1:18500/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(req)
	if err != nil {
		http.Error(w, "Browser unavailable", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		http.Error(w, "Browser unavailable", 502)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	flusher := w.(http.Flusher)
	flusher.Flush()
	_ = s.writeBrowserEvents(w, response.Body)
}

func vmAlive(profile, workspace string) bool {
	if workspace == "" {
		return false
	}
	b, e := os.ReadFile(filepath.Join(profile, "state", "lima", vmname.Name(workspace), "cage-running.json"))
	if e != nil {
		return false
	}
	var record struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(b, &record) != nil || record.PID < 1 {
		return false
	}
	return processExists(record.PID)
}
func (s *Server) vmRunning() bool { return vmAlive(s.profile, s.environmentDir()) }
