package workbench

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/vmname"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var assets embed.FS

const cookieName = "cage_session"

type controlFile struct {
	Port       int                `json:"port"`
	Token      string             `json:"token"`
	PID        int                `json:"pid"`
	Controller ControllerIdentity `json:"controller"`
}
type Job struct {
	pauseVersion uint64
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Running      bool   `json:"running"`
	Message      string `json:"message"`
	Error        string `json:"error,omitempty"`
	Log          string `json:"log,omitempty"`
}
type Server struct {
	identity                                         ControllerIdentity
	guestManifest                                    *cage.GuestManifest
	guestUpdateRequired                              bool
	guestUpdateError                                 string
	closing                                          bool
	shutdown                                         func()
	favoritesMu                                      sync.Mutex
	terminalSequence                                 int
	tickets                                          map[string]time.Time
	guestClient                                      *http.Client
	op                                               sync.Mutex
	profile, configPath, exe, origin, token, session string
	mu                                               sync.Mutex
	preferences                                      Preferences
	job                                              Job
	ready                                            bool
	location                                         *Location
	networkError                                     string
	credentials                                      guestCredentials
	terminals                                        map[string]*terminalSession
	projectShares                                    map[string]*projectShare
	projectError                                     string
	safety                                           safetyState
	locations                                        map[string]Location
	monitorCancel                                    context.CancelFunc
	safetyOp                                         sync.Mutex
	networkPaused                                    bool
	proxyNetworkLost                                 bool
	checkCancel                                      context.CancelFunc
	jobCancel                                        context.CancelFunc
	activeContext                                    context.Context
	activeJobID                                      string
	activePauseVersion                               uint64
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func equalSecret(a, b string) bool {
	return len(a) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func Serve(configPath string) error {
	profile := filepath.Dir(configPath)
	if err := host.EnsurePrivateDir(profile); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(profile, "control.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = lockController(lock); err != nil {
		return errors.New("此配置已有工作台服务正在运行")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	var listener net.Listener
	port := 17777
	for ; port < 17808; port++ {
		listener, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	defer listener.Close()
	s := &Server{profile: profile, configPath: configPath, exe: exe, origin: fmt.Sprintf("http://localhost:%d", port), token: randomToken(), session: randomToken(), preferences: Preferences{Keyboard: defaultKeyboard()}, terminals: map[string]*terminalSession{}}
	s.identity, err = currentController()
	if err != nil {
		return err
	}
	if b, e := os.ReadFile(filepath.Join(profile, "workbench.json")); e == nil {
		_ = json.Unmarshal(b, &s.preferences)
	}
	if err = s.prepareEnvironment(); err != nil {
		return err
	}
	if intentErr := s.restoreNetworkIntent(); intentErr != nil {
		s.networkError = intentErr.Error()
	}
	if s.preferences.Keyboard == "" {
		s.preferences.Keyboard = defaultKeyboard()
	}
	if _, e := os.Stat(configPath); os.IsNotExist(e) {
		if e = writePrivate(configPath, defaultConfig()); e != nil {
			return e
		}
	}
	if err = writePrivate(filepath.Join(profile, "control.json"), controlFile{Port: port, Token: s.token, PID: os.Getpid(), Controller: s.identity}); err != nil {
		return err
	}
	// A previously running VM cannot keep an unmanaged endpoint online.
	if cfg, e := loadConfig(configPath); e != nil || cfg.RequireManagedProxy(profile) != nil {
		_ = s.closeProxyGate("尚未配置有效节点，外网保持关闭")
	}
	if s.networkPaused {
		_ = s.closeProxyGate("网络保持暂停，请手动恢复后再联网")
	}
	go s.monitor()
	go s.monitorNetwork()
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	shutdownDone := make(chan struct{})
	s.shutdown = func() {
		defer close(shutdownDone)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = server.Close()
	}
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { jsonReply(w, map[string]bool{"ready": true}) })
	mux.HandleFunc("POST /api/launch", func(w http.ResponseWriter, r *http.Request) {
		if !s.launcherAuthenticated(w, r) {
			return
		}
		jsonReply(w, launchResponse{URL: s.origin + "/?launch=" + randomToken()[:8] + "#" + s.issueTicket(), Controller: s.identity})
	})
	mux.HandleFunc("POST /api/handoff", s.handoff)
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Token string `json:"token"`
		}
		if !readJSON(w, r, &v) {
			return
		}
		if !s.consumeTicket(v.Token) {
			http.Error(w, "访问凭证无效，请从 Cage 应用重新打开。", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: s.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400 * 30})
		jsonReply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("GET /api/environment/versions", s.environmentVersions)
	mux.HandleFunc("POST /api/environment/update", s.updateEnvironment)
	mux.HandleFunc("POST /api/environment/tools", s.updateEnvironmentTools)
	mux.HandleFunc("GET /api/host/git", s.hostGit)
	mux.HandleFunc("POST /api/host/git", s.enableHostGit)
	mux.HandleFunc("GET /api/appearance", s.getAppearance)
	mux.HandleFunc("GET /api/favorites", s.getFavorites)
	mux.HandleFunc("POST /api/favorites", s.mutateFavorites)
	mux.HandleFunc("POST /api/appearance", s.saveAppearance)
	mux.HandleFunc("GET /api/appearance/codex", s.codexAppearance)
	mux.HandleFunc("POST /api/settings/probe", s.probe)
	mux.HandleFunc("GET /api/proxy", s.proxyStatus)
	mux.HandleFunc("POST /api/proxy/delay", s.proxyDelay)
	mux.HandleFunc("GET /api/proxy/connections", s.proxyConnections)
	mux.HandleFunc("GET /api/proxy/logs", s.proxyLogs)
	mux.HandleFunc("POST /api/proxy/{action}", s.proxyMutation)
	mux.HandleFunc("POST /api/settings", s.saveSettings)
	mux.HandleFunc("POST /api/restore", s.restore)
	mux.HandleFunc("POST /api/start", s.start)
	mux.HandleFunc("POST /api/stop", s.stop)
	mux.HandleFunc("POST /api/safety/check", s.safetyCheck)
	mux.HandleFunc("POST /api/safety/close", s.pauseNetwork)
	mux.HandleFunc("GET /api/safety/report", s.safetyExport)
	mux.HandleFunc("GET /api/browser/tabs", s.browser)
	mux.HandleFunc("GET /api/browser/events", s.browserEvents)
	mux.HandleFunc("POST /api/browser/{action}", s.browser)
	mux.HandleFunc("GET /api/vnc-credentials", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.ready {
			http.Error(w, "环境尚未就绪", 409)
			return
		}
		jsonReply(w, map[string]string{"password": s.credentials.VNCPassword})
	})
	mux.HandleFunc("GET /api/vnc", s.vnc)
	mux.HandleFunc("GET /api/terminals", s.listTerminals)
	mux.HandleFunc("POST /api/terminals", s.newTerminal)
	mux.HandleFunc("DELETE /api/terminals/{id}", s.closeTerminal)
	mux.HandleFunc("GET /api/terminals/{id}/stream", s.terminalStream)
	web, _ := fs.Sub(assets, "web")
	mux.Handle("GET /", http.FileServer(http.FS(web)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin, err := url.Parse(s.origin)
		if err != nil || r.Host != origin.Host {
			http.Error(w, "Host refused", 403)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self' ws://"+origin.Host+"; img-src 'self' data:; font-src 'self'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		if (r.Method != "GET" && r.Method != "HEAD") || strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			if r.Header.Get("Origin") != s.origin {
				http.Error(w, "Origin refused", 403)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/session" && r.URL.Path != "/api/launch" && r.URL.Path != "/api/handoff" {
			c, e := r.Cookie(s.cookieName())
			if e != nil || !equalSecret(c.Value, s.session) {
				http.Error(w, "请从 Cage 应用重新打开工作台", 401)
				return
			}
		}
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			http.Error(w, "后台正在更新，请重新打开 Cage。", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func readJSON(w http.ResponseWriter, r *http.Request, v any, limits ...int64) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "需要 JSON 请求", 415)
		return false
	}
	limit := int64(64 << 10)
	if len(limits) > 0 {
		limit = limits[0]
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "请求格式无效", 400)
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		http.Error(w, "请求格式无效", 400)
		return false
	}
	return true
}
func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func requestError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusBadRequest)
}
func (s *Server) busy() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.job.Running }
func (s *Server) launch(w http.ResponseWriter, kind string, fn func() error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		http.Error(w, "后台正在更新，请重新打开 Cage。", http.StatusServiceUnavailable)
		return
	}
	if s.job.Running {
		if kind == "stop" && s.job.Kind == "stop" {
			job := s.job
			s.mu.Unlock()
			jsonReply(w, job)
			return
		}
		if kind != "stop" {
			s.mu.Unlock()
			http.Error(w, "另一个操作仍在进行，请稍候", 409)
			return
		}
		if s.jobCancel != nil {
			s.jobCancel()
		}
	}
	if kind == "start" || kind == "safety" || kind == "restore" {
		if err := s.saveNetworkIntentLocked(false); err != nil {
			s.mu.Unlock()
			_ = s.closeProxyGate("网络恢复意图无法保存，外网保持关闭")
			requestError(w, err)
			return
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.jobCancel = cancel
	s.job = Job{ID: randomToken()[:12], Kind: kind, Running: true, Message: "正在准备…", pauseVersion: s.safety.pauseVersion}
	if s.monitorCancel != nil {
		s.monitorCancel()
	}
	job := s.job
	s.mu.Unlock()
	go func() {
		defer cancel()
		s.op.Lock()
		defer s.op.Unlock()
		s.mu.Lock()
		if s.job.ID != job.ID || ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		s.activeContext, s.activeJobID = ctx, job.ID
		s.activePauseVersion = job.pauseVersion
		s.mu.Unlock()
		err := fn()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.activeContext, s.activeJobID = nil, ""
		s.activePauseVersion = 0
		if s.job.ID != job.ID {
			return
		}
		s.jobCancel = nil
		s.job.Running = false
		if errors.Is(err, context.Canceled) {
			s.job.Message = "已取消"
		} else if err != nil {
			s.job.Error = err.Error()
			s.job.Message = "操作未完成"
		} else {
			s.job.Message = "已完成"
		}
	}()
	jsonReply(w, job)
}
func (s *Server) setMessage(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeJobID != "" && s.activeJobID != s.job.ID {
		return
	}
	s.job.Message = v
}

func (s *Server) operationContext() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeContext != nil {
		return s.activeContext
	}
	return context.Background()
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	projects, err := s.projectPaths()
	if err != nil {
		requestError(w, errors.New("无法读取项目列表，请检查项目配置"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, e := os.Stat(filepath.Join(s.profile, "previous-profile.json"))
	jsonReply(w, map[string]any{"projects": projects, "ready": s.ready, "vm_running": vmAlive(s.profile, s.environmentDir()), "workspace": s.preferences.Workspace, "environment_workspace": s.preferences.Workspace == s.environmentDir(), "environment_directory": s.environmentDir(), "project_error": s.projectError, "keyboard": s.preferences.Keyboard, "auto_timezone": s.preferences.AutoTimezone, "job": s.job, "location": s.location, "network_error": s.networkError, "can_restore": e == nil, "safety": s.safety, "environment_update_required": s.guestUpdateRequired, "environment_update_error": s.guestUpdateError})
}
func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	c, err := loadConfig(s.configPath)
	if err != nil {
		requestError(w, err)
		return
	}
	s.mu.Lock()
	p := s.preferences
	s.mu.Unlock()
	jsonReply(w, settingsView(c, p))
}
func (s *Server) proposed(w http.ResponseWriter, r *http.Request) (savedProfile, bool) {
	var input Settings
	if !readJSON(w, r, &input) {
		return savedProfile{}, false
	}
	old, err := loadConfig(s.configPath)
	if err != nil {
		requestError(w, err)
		return savedProfile{}, false
	}
	c, p, err := proposedSettings(input, old, s.profile)
	if err != nil {
		requestError(w, err)
		return savedProfile{}, false
	}
	return savedProfile{c, p}, true
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	v, ok := s.proposed(w, r)
	if !ok {
		return
	}
	if err := v.Config.RequireManagedProxy(s.profile); err != nil {
		requestError(w, err)
		return
	}
	loc, err := probeLocation(r.Context(), v.Config)
	if err != nil {
		requestError(w, err)
		return
	}
	current, _ := loadConfig(s.configPath)
	if current.ProxyURL == v.Config.ProxyURL && current.DNSURL == v.Config.DNSURL {
		s.mu.Lock()
		s.location = &loc
		s.networkError = ""
		s.mu.Unlock()
	}
	jsonReply(w, loc)
}
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	v, ok := s.proposed(w, r)
	if !ok {
		return
	}
	kind := "settings"
	old, err := loadConfig(s.configPath)
	s.mu.Lock()
	previous := s.preferences
	s.mu.Unlock()
	if err == nil && old == v.Config && previous.AutoTimezone == v.Preferences.AutoTimezone && previous.AutoLanguage == v.Preferences.AutoLanguage && previous.Workspace != v.Preferences.Workspace {
		kind = "project"
	}
	s.launch(w, kind, func() error { return s.apply(v, true) })
}
func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var v savedProfile
	b, e := os.ReadFile(filepath.Join(s.profile, "previous-profile.json"))
	if e != nil || json.Unmarshal(b, &v) != nil {
		requestError(w, errors.New("没有可恢复的配置"))
		return
	}
	s.launch(w, "restore", func() error {
		if err := s.apply(v, false); err != nil {
			return err
		}
		return s.startEnvironment(false)
	})
}
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	s.launch(w, "start", func() error {
		if err := s.prepareEnvironment(); err != nil {
			return err
		}
		return s.startEnvironment(true)
	})
}
func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{vmname.Name(s.environmentDir()), "base-building"} {
		if owner := cage.InstanceJobID(filepath.Join(s.profile, "state"), name); owner != "" {
			_ = cage.RequestJobStop(s.profile, owner)
		}
	}
	s.mu.Lock()
	jobID := s.activeJobID
	if s.job.Kind != "stop" && s.jobCancel != nil {
		s.jobCancel()
	}
	s.mu.Unlock()
	if jobID != "" {
		_ = cage.RequestJobStop(s.profile, jobID)
	}
	_ = s.pauseNetworkNow("正在停止环境")
	s.launch(w, "stop", s.stopEnvironment)
}

// Short-lived contexts for guest controls are separate from VM lifecycle jobs.
func controlContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (s *Server) cookieName() string { u, _ := url.Parse(s.origin); return cookieName + "_" + u.Port() }

func (s *Server) issueTicket() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tickets == nil {
		s.tickets = map[string]time.Time{}
	}
	for token, expires := range s.tickets {
		if time.Now().After(expires) {
			delete(s.tickets, token)
		}
	}
	token := randomToken()
	s.tickets[token] = time.Now().Add(time.Minute)
	return token
}
func (s *Server) consumeTicket(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.tickets[token]
	delete(s.tickets, token)
	return ok && time.Now().Before(expires)
}
