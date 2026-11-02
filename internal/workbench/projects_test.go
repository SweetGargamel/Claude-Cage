package workbench

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"os"
	"path/filepath"
	"testing"

	"github.com/lima-vm/sshocker/pkg/reversesshfs"
	"github.com/pkg/sftp"
	"github.com/sylvinus/agent-vm/internal/mounts"
	"github.com/sylvinus/agent-vm/internal/vmname"
)

func TestProjectSelectionKeepsOneVMAndExistingSessionsOffline(t *testing.T) {
	s := testServer(t)
	cfg, _ := loadConfig(s.configPath)
	s.preferences.Workspace = s.environmentDir()
	s.ready, s.networkPaused = true, true
	marker := filepath.Join(s.profile, "state", "lima", vmname.Name(s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	originalVM := vmname.Name(s.environmentDir())
	terminal := &terminalSession{id: "keep", title: "existing terminal"}
	s.terminals[terminal.id] = terminal
	a, _ := filepath.EvalSymlinks(t.TempDir())
	b, _ := filepath.EvalSymlinks(t.TempDir())
	s.projectShares = map[string]*projectShare{a: {done: make(chan struct{})}, b: {done: make(chan struct{})}}
	for _, dir := range []string{a, b, a} {
		next := s.preferences
		next.Workspace = dir
		// The configured proxy is unavailable. Project selection must not probe it.
		if err := s.apply(savedProfile{cfg, next}, true); err != nil {
			t.Fatal(err)
		}
		if !s.ready || !s.vmRunning() || !s.networkPaused || vmname.Name(s.environmentDir()) != originalVM {
			t.Fatal("project selection changed environment lifecycle or networking")
		}
		if s.terminals[terminal.id] != terminal || terminal.closed {
			t.Fatal("project selection interrupted the old terminal")
		}
	}
	var saved sharedProjects
	data, err := os.ReadFile(filepath.Join(s.profile, "projects.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || len(saved.Paths) != 2 {
		t.Fatal("explicitly opened projects were not persisted uniquely")
	}
	next := s.preferences
	next.Workspace = s.profile
	if err := s.apply(savedProfile{cfg, next}, true); err == nil {
		t.Fatal("private directory accepted as a project")
	}
	if s.workspace() != a || terminal.closed || !s.ready {
		t.Fatal("failed project selection changed the active environment")
	}
}

func TestLiveProjectTransportConfinesReadsAndProtectsMetadata(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	server, conn := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); _ = reversesshfs.ServeConfined(server, root, false, mounts.BaseNames) }()
	client, err := sftp.NewClientPipe(conn, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(); _ = conn.Close(); <-done }()
	file, err := client.Create(filepath.Join(root, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("shared")); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	data, err := os.ReadFile(filepath.Join(root, "note.txt"))
	if err != nil || string(data) != "shared" {
		t.Fatal("project write did not reach the host")
	}
	for _, p := range []string{outside, filepath.Join(root, "escape", "secret")} {
		f, err := client.Open(p)
		if err == nil {
			_ = f.Close()
			t.Fatalf("read escaped selected project: %s", p)
		}
	}
	if f, err := client.Create(filepath.Join(root, ".git", "config")); err == nil {
		_ = f.Close()
		t.Fatal("Git metadata was writable")
	}
	f, err := client.Open(filepath.Join(root, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "shared" {
		t.Fatal("shared file read failed")
	}
}

func TestProjectListComesFromSavedDirectories(t *testing.T) {
	s := testServer(t)
	var expected []string
	expected = append(expected, s.environmentDir())
	for i := 0; i < 15; i++ {
		path := t.TempDir()
		if err := s.rememberSharedProject(path); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, path)
	}
	s.preferences.Workspace = expected[2]
	if err := s.rememberSharedProject(expected[2]); err != nil {
		t.Fatal(err)
	}
	// Reload from the same profile, without any browser-local history.
	reloaded := testServer(t)
	reloaded.profile = s.profile
	reloaded.preferences = s.preferences
	w := request(reloaded, "GET", "/api/status", "", "", true)
	var status struct {
		Projects []string `json:"projects"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || !slices.Equal(status.Projects, expected) {
		t.Fatalf("saved project list was truncated, reordered, or lost: %s", w.Body.String())
	}
}

func TestProjectActionsUseExplicitDirectoryWithoutSwitchingProject(t *testing.T) {
	s := testServer(t)
	a, _ := filepath.EvalSymlinks(t.TempDir())
	b, _ := filepath.EvalSymlinks(t.TempDir())
	s.preferences.Workspace = b
	s.ready = true
	if err := s.rememberSharedProject(a); err != nil {
		t.Fatal(err)
	}
	s.projectShares = map[string]*projectShare{a: {done: make(chan struct{})}}
	marker := filepath.Join(s.profile, "state", "lima", vmname.Name(s.environmentDir()), "cage-running.json")
	if err := writePrivate(marker, map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	// The absent SSH config makes this local child exit before connecting.
	body, _ := json.Marshal(map[string]string{"workspace": a})
	w := request(s, "POST", "/api/terminals", s.origin, string(body), true)
	var terminal struct{ ID, Workspace string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &terminal) != nil || terminal.Workspace != a {
		t.Fatalf("terminal did not use the explicit directory: %s", w.Body.String())
	}
	session := s.terminals[terminal.ID]
	defer session.close()
	if script := session.cmd.Args[len(session.cmd.Args)-1]; !strings.HasPrefix(script, "cd "+shellescape.Quote(guestPath(a))+" &&") {
		t.Fatalf("terminal command opened a different directory: %s", script)
	}
	var editorWorkspace string
	s.guestClient = &http.Client{Transport: browserTransport(func(r *http.Request) (*http.Response, error) {
		var input struct {
			Workspace string `json:"workspace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		editorWorkspace = input.Workspace
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"editor"}`)), Header: http.Header{}}, nil
	})}
	for _, action := range []string{"editor", "editor-restart"} {
		w = request(s, "POST", "/api/browser/"+action, s.origin, string(body), true)
		if w.Code != 200 || editorWorkspace != guestPath(a) || s.workspace() != b {
			t.Fatalf("%s changed the selected project or ignored its directory: %s", action, w.Body.String())
		}
	}
	for _, path := range []string{t.TempDir(), s.profile} {
		body, _ = json.Marshal(map[string]string{"workspace": path})
		for _, endpoint := range []string{"/api/terminals", "/api/browser/editor", "/api/browser/editor-restart"} {
			w = request(s, "POST", endpoint, s.origin, string(body), true)
			if w.Code != 400 {
				t.Fatalf("unopened directory accepted by %s: %d", endpoint, w.Code)
			}
		}
	}
	if len(s.terminals) != 1 || s.workspace() != b {
		t.Fatal("rejected requests changed existing sessions")
	}
}
