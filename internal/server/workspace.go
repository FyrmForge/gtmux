package server

import (
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/FyrmForge/gtmux/internal/proto"
	"github.com/FyrmForge/gtmux/internal/workspace"
)

// workspaceOpen resolves (cloning if needed) and builds a project's session,
// returning its name. An existing session is reused untouched. Blocking: call
// from a connection goroutine or a spawned goroutine, never a session's owner
// goroutine (it drives the new session via s.command).
func (r *registry) workspaceOpen(ws, project string, env map[string]string) (string, error) {
	w, err := r.ws.Config().Get(ws)
	if err != nil {
		return "", err
	}
	if w.Name == "temp" && project == "" {
		// Next numbered temp: pick + mkdir under one lock, so two at once
		// don't both take "1".
		tl := r.openLock("temp\x00")
		tl.Lock()
		defer tl.Unlock()
		project = r.ws.NextTemp(r.exists)
	}
	// One open per project at a time: a second request waits, then finds the
	// finished session instead of a half-built one or a mid-flight clone.
	mu := r.openLock(w.Name + "\x00" + project)
	mu.Lock()
	defer mu.Unlock()
	// Already open: switch, even if its folder/layout would no longer prepare.
	if name := workspace.SessionName(w, project); project != "" && r.exists(name) {
		return r.adopt(w.Name, project, name)
	}
	spec, err := r.ws.Prepare(w.Name, project, env)
	if err != nil {
		return "", err
	}
	return r.build(w.Name, spec)
}

// workspaceNew scaffolds a project (workspace.Create) and builds its session.
func (r *registry) workspaceNew(ws string, o workspace.NewOpts) (string, error) {
	w, err := r.ws.Config().Get(ws)
	if err != nil {
		return "", err
	}
	mu := r.openLock(w.Name + "\x00" + o.Name)
	mu.Lock()
	defer mu.Unlock()
	// build() would reuse it and the scaffold chain would never be typed:
	// refuse before anything (a remote repo) is created.
	name := workspace.SessionName(w, o.Name)
	if _, ok := r.get(name); ok {
		return "", fmt.Errorf("session %s already exists", name)
	}
	spec, err := r.ws.Create(w.Name, o)
	if err != nil {
		return "", err
	}
	return r.build(w.Name, spec)
}

// build creates spec's session, or reuses an existing one of the same workspace.
func (r *registry) build(ws string, spec *workspace.SessionSpec) (string, error) {
	if r.exists(spec.Name) {
		return r.adopt(ws, spec.Project, spec.Name)
	}
	// The first pane spawns with the session, in the first window's dir, with
	// the workspace env already set (no respawn).
	s, err := r.createWithEnv(spec.Name, 80, 24, spec.Windows[0].Dir, spec.Env)
	if err != nil {
		if _, ok := r.get(spec.Name); ok { // lost a create race: same result
			return spec.Name, nil
		}
		return "", err
	}
	buildSession(s, spec)
	select {
	case <-s.gone: // its first pane failed to start (bad dir/shell): say so
		return "", fmt.Errorf("session %s exited while starting (first window dir %s; see the server log)", spec.Name, spec.Windows[0].Dir)
	default:
	}
	return spec.Name, nil
}

// adopt returns an existing session for workspace ws, refusing one that
// belongs to another workspace (two workspaces sharing a prefix map a project
// to the same <prefix>/<project>). An untagged one (made by hand) is
// tagged, so it counts as this session's workspace from now on.
func (r *registry) adopt(ws, project, name string) (string, error) {
	owner := r.sessionWorkspace(name)
	if owner != "" && owner != ws {
		return "", fmt.Errorf("session %s belongs to workspace %s", name, owner)
	}
	if owner == "" {
		if s, ok := r.get(name); ok {
			s.command([]string{"set-option", "@workspace", ws})
			s.command([]string{"set-option", "@project", project})
		}
	}
	return name, nil
}

// removeTemp deletes a temp workspace's folder, refused while its session is
// open (its shells would be left in a deleted dir): by name, or by tag, so a
// renamed one counts. Under the open lock, so an open can't land mid-delete.
// Queries every session: never call it on a session's owner goroutine.
func (r *registry) removeTemp(name string) error {
	mu := r.openLock("temp\x00" + name)
	mu.Lock()
	defer mu.Unlock()
	w, _ := r.ws.Config().Get("temp")
	if s := workspace.SessionName(w, name); r.exists(s) {
		return fmt.Errorf("session %s is open; close it first", s)
	}
	for _, s := range r.names() {
		if r.sessionOpt(s, "#{@workspace}/#{@project}") == "temp/"+name {
			return fmt.Errorf("session %s is open on it; close it first", s)
		}
	}
	return r.ws.RemoveTemp(name)
}

func (r *registry) exists(name string) bool { _, ok := r.get(name); return ok }

// openLock returns the per-project open mutex. ponytail: entries are never
// freed — one tiny mutex per project ever opened, fine for a daemon's life.
func (r *registry) openLock(key string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.opening == nil {
		r.opening = map[string]*sync.Mutex{}
	}
	m := r.opening[key]
	if m == nil {
		m = &sync.Mutex{}
		r.opening[key] = m
	}
	return m
}

// buildSession shapes a fresh session with the same commands `gtmux run` would
// send (workspacer's proven gtmux sequence). Pane commands are TYPED into the
// shell (send-keys), not spawned, so quitting nvim leaves a shell behind.
// Best-effort per step, like workspacer: a failed step is logged, not fatal.
func buildSession(s *session, spec *workspace.SessionSpec) {
	run := func(args ...string) {
		if _, errText := s.command(args); errText != "" {
			log.Printf("gtmux workspace %s: %s: %s", spec.Name, strings.Join(args, " "), errText)
		}
	}
	run("set-option", "@workspace", spec.Workspace)
	run("set-option", "@project", spec.Project)
	for i, w := range spec.Windows {
		if i == 0 {
			if w.Name != "" {
				run("rename-window", w.Name)
			}
		} else {
			args := []string{"new-window", "-c", w.Dir}
			if w.Name != "" {
				args = append(args, "-n", w.Name)
			}
			run(args...)
		}
		for pi, p := range w.Panes {
			if pi > 0 {
				args := []string{"split-window", "-c", w.Dir}
				if p.Split == "h" {
					args = append(args, "-h")
				}
				run(args...)
			}
			if p.Cmd != "" {
				run("send-keys", "-l", p.Cmd)
				run("send-keys", "Enter")
			}
			if p.Size > 0 && p.Size < 100 && pi > 0 {
				flag := "-y"
				if p.Split == "h" {
					flag = "-x"
				}
				run("resize-pane", flag, fmt.Sprintf("%d%%", p.Size))
			}
		}
		if w.SelectLayout != "" {
			run("select-layout", w.SelectLayout)
		}
	}
	run("select-window", "-t", "{start}")
}

// sessionWorkspace reads a session's @workspace tag ("" if untagged/unknown).
func (r *registry) sessionWorkspace(name string) string {
	return r.sessionOpt(name, "#{@workspace}")
}

// sessionOpt expands format f in session name ("" if unknown).
func (r *registry) sessionOpt(name, f string) string {
	if name == "" {
		return ""
	}
	s, ok := r.get(name)
	if !ok {
		return ""
	}
	out, _ := s.command([]string{"display-message", "-p", f})
	return strings.TrimSpace(out)
}

// handleWorkspace answers a one-shot WorkspaceRequest (connection goroutine).
func (r *registry) handleWorkspace(req *proto.WorkspaceRequest) (*proto.WorkspaceReply, error) {
	cfg := r.ws.Config()
	name := req.Workspace
	if name == "" {
		name = r.sessionWorkspace(req.Session)
	}
	switch req.Op {
	case "list":
		w, rows, status, err := r.ws.Projects(name, func(s string) bool { _, ok := r.get(s); return ok })
		if err != nil {
			return nil, err
		}
		rep := &proto.WorkspaceReply{Workspace: w.Name, Workspaces: cfg.Names(), Status: status, Account: w.Account != ""}
		for _, t := range w.Templates {
			rep.Templates = append(rep.Templates, t.Name)
		}
		for _, row := range rows {
			rep.Projects = append(rep.Projects, proto.WorkspaceProject{
				Name: row.Name, Description: row.Description, Local: row.Local, Remote: row.Remote, Open: row.Open,
			})
		}
		return rep, nil
	case "open":
		sess, err := r.workspaceOpen(name, req.Project, req.Env)
		if err != nil {
			return nil, err
		}
		return &proto.WorkspaceReply{Session: sess}, nil
	case "new":
		sess, err := r.workspaceNew(name, workspace.NewOpts{
			Name: req.Project, Template: req.Template, GitHub: req.GitHub, Private: req.Private, Env: req.Env,
		})
		if err != nil {
			return nil, err
		}
		return &proto.WorkspaceReply{Session: sess}, nil
	case "temp-rm":
		return &proto.WorkspaceReply{}, r.removeTemp(req.Project)
	case "refresh":
		return &proto.WorkspaceReply{}, r.ws.Refresh(name)
	}
	return nil, fmt.Errorf("workspace: unknown op %q", req.Op)
}
