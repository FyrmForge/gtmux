package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Repo is one remote repository as the picker needs it.
type Repo struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	PushedAt    time.Time `json:"pushed_at"`
	Archived    bool      `json:"archived,omitempty"`
}

// errUnauthorized lets the actor drop its cached token and re-resolve once.
var errUnauthorized = errors.New("github: 401 unauthorized")

// resolveToken runs the account's token source. Called off the Lua VM, on a
// fetch goroutine; the daemon has no TTY, so cmd_auth must not prompt on one.
func resolveToken(a Auth) (string, error) {
	var out []byte
	var err error
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch a.Kind {
	case "env":
		if t := strings.TrimSpace(os.Getenv(a.Arg)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("auth: $%s is empty in the daemon's environment (set before the daemon started?)", a.Arg)
	case "ghcli":
		args := []string{"auth", "token"}
		if a.Host != "" {
			args = append(args, "--hostname", a.Host)
		}
		if a.Arg != "" {
			args = append(args, "--user", a.Arg)
		}
		out, err = exec.CommandContext(ctx, "gh", args...).Output()
	case "cmd":
		out, err = exec.CommandContext(ctx, "sh", "-c", a.Arg).Output()
	default:
		return "", fmt.Errorf("auth: unknown kind %q", a.Kind)
	}
	if err != nil {
		return "", fmt.Errorf("auth (%s): %w", a.Kind, err)
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", fmt.Errorf("auth (%s): empty token", a.Kind)
	}
	return t, nil
}

func (a *Account) api() string {
	if a.apiURL != "" {
		return a.apiURL
	}
	if a.Host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + a.Host + "/api/v3" // GitHub Enterprise Server
}

// firstPageURL lists repos newest-pushed first. Orgs: every repo the token can
// see. Users: /user/repos?affiliation=owner, which includes private repos but
// only when the token belongs to that user (/users/{login}/repos is public-only).
func (a *Account) firstPageURL() string {
	q := "sort=pushed&direction=desc&per_page=100"
	if a.Kind == "org" {
		return a.api() + "/orgs/" + a.Login + "/repos?type=all&" + q
	}
	return a.api() + "/user/repos?affiliation=owner&" + q
}

// page is one fetched page of repos plus where to go next.
type page struct {
	repos       []Repo
	next        string // "" = last page
	etag        string
	notModified bool
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func fetchPage(url, token, etag string) (page, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return page{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "gtmux")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return page{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return page{notModified: true, etag: etag}, nil
	case http.StatusUnauthorized:
		return page{}, errUnauthorized
	case http.StatusOK:
	default:
		return page{}, fmt.Errorf("github: %s: %s", url, resp.Status)
	}
	var raw []struct {
		Name        string    `json:"name"`
		Description string    `json:"description"`
		PushedAt    time.Time `json:"pushed_at"`
		Archived    bool      `json:"archived"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return page{}, fmt.Errorf("github: decode: %w", err)
	}
	p := page{etag: resp.Header.Get("ETag")}
	if m := linkNext.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		p.next = m[1]
	}
	for _, r := range raw {
		p.repos = append(p.repos, Repo{Name: r.Name, Description: r.Description, PushedAt: r.PushedAt, Archived: r.Archived})
	}
	return p, nil
}

// fetchMsg is one step of a refresh, streamed to the actor so the picker sees
// pages as they land. done ends the refresh; full marks a complete sweep (the
// actor then drops repos the sweep didn't see — deletes/renames).
type fetchMsg struct {
	repos []Repo
	etag  string // page-1 ETag, set on the first page message
	token string // resolved token, for the actor to cache
	done  bool
	full  bool
	err   error
}

// fetch runs one refresh and streams pages to out.
//
// Incremental (full=false): page 1 goes out with the cached ETag — a 304 means
// nothing was pushed since, and costs no rate limit. Otherwise keep paging
// (newest-pushed first) until a page holds a repo pushed at or before the
// high-water mark hwm: everything older is already cached. Page-boundary
// shifts don't matter because the stop test is on push time, not page content.
func fetch(a *Account, token, etag string, hwm time.Time, full bool, out chan<- fetchMsg) {
	send := func(m fetchMsg) { out <- m }
	var err error
	if token == "" {
		if token, err = resolveToken(a.Auth); err != nil {
			send(fetchMsg{done: true, err: err})
			return
		}
	}
	if full {
		etag = ""
	}
	url, first := a.firstPageURL(), true
	for url != "" {
		tag := ""
		if first {
			tag = etag
		}
		p, err := fetchPage(url, token, tag)
		if err != nil {
			send(fetchMsg{done: true, err: err})
			return
		}
		m := fetchMsg{repos: p.repos}
		if first {
			m.etag, m.token = p.etag, token
			first = false
		}
		if p.notModified {
			m.done = true
			send(m)
			return
		}
		stop := false
		if !full && !hwm.IsZero() {
			for _, r := range p.repos {
				if !r.PushedAt.After(hwm) {
					stop = true
					break
				}
			}
		}
		url = p.next
		if stop {
			url = ""
		}
		if url == "" {
			m.done, m.full = true, full || !stop // never stopped early = saw every page
		}
		send(m)
	}
}

// createRepo makes an empty repo (no auto-init: the first push sets the default
// branch). Orgs: POST /orgs/{login}/repos. Users: POST /user/repos, which
// creates under the TOKEN's owner — Create checks it against login first.
func createRepo(a *Account, token, name string, private bool) (Repo, error) {
	url := a.api() + "/user/repos"
	if a.Kind == "org" {
		url = a.api() + "/orgs/" + a.Login + "/repos"
	}
	body, _ := json.Marshal(map[string]any{"name": name, "private": private, "auto_init": false})
	req, err := http.NewRequest("POST", url, strings.NewReader(string(body)))
	if err != nil {
		return Repo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "gtmux")
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return Repo{}, err
	}
	defer resp.Body.Close()
	var out struct {
		Name     string    `json:"name"`
		PushedAt time.Time `json:"pushed_at"`
		Message  string    `json:"message"`
		Errors   []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	switch resp.StatusCode {
	case http.StatusCreated:
		if out.Name == "" {
			out.Name = name
		}
		return Repo{Name: out.Name, PushedAt: out.PushedAt}, nil
	case http.StatusUnauthorized:
		return Repo{}, errUnauthorized
	case http.StatusForbidden, http.StatusNotFound:
		return Repo{}, fmt.Errorf("token for account %s can't create repos (%s)", a.Name, resp.Status)
	}
	msg := out.Message
	for _, e := range out.Errors {
		msg += ": " + e.Message
	}
	return Repo{}, fmt.Errorf("github: create %s: %s: %s", name, resp.Status, msg)
}

// tokenLogin is the login owning token (GET /user).
func tokenLogin(a *Account, token string) (string, error) {
	req, err := http.NewRequest("GET", a.api()+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "gtmux")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: GET /user: %s", resp.Status)
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return "", fmt.Errorf("github: decode /user: %w", err)
	}
	return u.Login, nil
}
