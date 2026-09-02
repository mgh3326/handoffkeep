package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	hkmcp "github.com/mgh3326/handoffkeep/internal/mcp"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "handoffkeep:", err)
		os.Exit(1)
	}
}
func run(args []string, out, errout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: handoffkeep serve|mcp|ctx|memory|doc")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], errout)
	case "mcp":
		return runMCP()
	case "ctx":
		return ctxCmd(args[1:], out)
	case "memory":
		return memoryCmd(args[1:], out)
	case "doc":
		return docCmd(args[1:], out)
	default:
		return errors.New("usage: handoffkeep serve|mcp|ctx|memory|doc")
	}
}

func config() (string, string) {
	u, t := os.Getenv("HANDOFFKEEP_URL"), os.Getenv("HANDOFFKEEP_TOKEN")
	if u != "" && t != "" {
		return u, t
	}
	home, _ := os.UserHomeDir()
	b, _ := os.ReadFile(filepath.Join(home, ".config/handoffkeep/config.env"))
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok {
			if k == "HANDOFFKEEP_URL" && u == "" {
				u = v
			}
			if k == "HANDOFFKEEP_TOKEN" && t == "" {
				t = v
			}
		}
	}
	return u, t
}
func remoteClient(fs *flag.FlagSet) *remote.Client {
	u, t := config()
	c := &remote.Client{}
	fs.StringVar(&c.URL, "url", u, "handoffkeep HTTP URL")
	fs.StringVar(&c.Token, "token", t, "handoffkeep bearer token")
	return c
}
func mustClient(c *remote.Client) error {
	if c.URL == "" || c.Token == "" {
		return errors.New("HANDOFFKEEP_URL and HANDOFFKEEP_TOKEN are required (or ~/.config/handoffkeep/config.env)")
	}
	return nil
}
func printJSON(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }
func ctxCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: ctx checkpoint|recent|search")
	}
	fs := flag.NewFlagSet("ctx "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	session := fs.String("session", "", "session")
	limit := fs.Int("limit", 3, "limit")
	kind := fs.String("kind", "", "checkpoint kind")
	title := fs.String("title", "", "title")
	body := fs.String("body", "", "body")
	file := fs.String("file", "", "body file")
	scope := fs.String("scope", "all", "scope")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if e := mustClient(c); e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "checkpoint":
		if *kind == "" {
			*kind = "checkpoint"
		}
		if *file != "" {
			b, e := os.ReadFile(*file)
			if e != nil {
				return e
			}
			*body = string(b)
		}
		v, e := c.Checkpoint(ctx, "", store.Checkpoint{Session: *session, Kind: *kind, Title: *title, Body: *body})
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "recent":
		v, e := c.Recent(ctx, *session, *kind, *limit)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "search":
		if fs.NArg() != 1 {
			return errors.New("ctx search requires query")
		}
		v, e := c.Search(ctx, fs.Arg(0), *scope, *session, *limit)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	default:
		return errors.New("usage: ctx checkpoint|recent|search")
	}
}

func memoryCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: memory push|pull|list")
	}
	fs := flag.NewFlagSet("memory "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	agent := fs.String("agent", "", "agent")
	dir := fs.String("dir", "", "directory")
	apply := fs.Bool("apply", false, "apply changes")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if e := mustClient(c); e != nil {
		return e
	}
	ctx := context.Background()
	switch args[0] {
	case "list":
		v, e := c.ListMemory(ctx, *agent, false)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "push":
		items, e := readMem(*agent, *dir)
		if e != nil {
			return e
		}
		if !*apply {
			return printJSON(out, map[string]any{"dry_run": true, "count": len(items)})
		}
		for _, x := range items {
			if _, e = c.PutMemory(ctx, "", x); e != nil {
				return e
			}
		}
		return printJSON(out, map[string]any{"pushed": len(items)})
	case "pull":
		items, e := c.ListMemory(ctx, *agent, true)
		if e != nil {
			return e
		}
		if !*apply {
			return printJSON(out, map[string]any{"dry_run": true, "count": len(items)})
		}
		if e = writeMem(*dir, items); e != nil {
			return e
		}
		return printJSON(out, map[string]any{"pulled": len(items)})
	default:
		return errors.New("usage: memory push|pull|list")
	}
}
func readMem(agent, dir string) ([]store.Memory, error) {
	ents, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	out := []store.Memory{}
	for _, x := range ents {
		if x.IsDir() || !strings.HasSuffix(x.Name(), ".md") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(dir, x.Name()))
		if e != nil {
			return nil, e
		}
		m, e := parseMem(agent, strings.TrimSuffix(x.Name(), ".md"), string(b))
		if e != nil {
			return nil, e
		}
		out = append(out, m)
	}
	return out, nil
}
func parseMem(agent, fallback, raw string) (store.Memory, error) {
	m := store.Memory{Agent: agent, Name: fallback, Type: "project", Content: raw}
	if !strings.HasPrefix(raw, "---\n") {
		return m, nil
	}
	parts := strings.SplitN(raw, "---\n", 3)
	if len(parts) != 3 {
		return m, errors.New("invalid memory frontmatter")
	}
	m.Content = parts[2]
	for _, l := range strings.Split(parts[1], "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			m.Name = strings.TrimSpace(v)
		case "description":
			m.Description = strings.TrimSpace(v)
		case "type":
			m.Type = strings.TrimSpace(v)
		}
	}
	return m, nil
}
func writeMem(dir string, items []store.Memory) error {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	names := []string{}
	for _, x := range items {
		raw := fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n---\n%s", x.Name, x.Description, x.Type, x.Content)
		if e := os.WriteFile(filepath.Join(dir, x.Name+".md"), []byte(raw), 0600); e != nil {
			return e
		}
		names = append(names, fmt.Sprintf("- [%s](%s.md) — %s", x.Name, x.Name, x.Description))
	}
	sort.Strings(names)
	return os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(strings.Join(names, "\n")+"\n"), 0600)
}

func docCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: doc put|get|list|import")
	}
	fs := flag.NewFlagSet("doc "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	key := fs.String("key", "", "key")
	kind := fs.String("kind", "", "kind")
	session := fs.String("session", "", "session")
	job := fs.String("job", "", "job")
	file := fs.String("file", "", "file")
	prefix := fs.String("prefix", "", "prefix")
	dir := fs.String("dir", "", "directory")
	glob := fs.String("glob", "**/*.md", "glob")
	apply := fs.Bool("apply", false, "apply changes")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if e := mustClient(c); e != nil {
		return e
	}
	ctx := context.Background()
	switch args[0] {
	case "put":
		if *kind == "" {
			*kind = "note"
		}
		b, e := os.ReadFile(*file)
		if e != nil {
			return e
		}
		v, changed, e := c.PutDocument(ctx, "", store.Document{Key: *key, Kind: *kind, Session: *session, Job: *job, Body: string(b)})
		if e != nil {
			return e
		}
		return printJSON(out, map[string]any{"document": v, "changed": changed})
	case "get":
		if fs.NArg() != 1 {
			return errors.New("doc get requires key")
		}
		v, ok, e := c.GetDocument(ctx, fs.Arg(0))
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("not_found")
		}
		return printJSON(out, v)
	case "list":
		v, e := c.ListDocuments(ctx, *prefix, *kind, *session, 100)
		if e != nil {
			return e
		}
		return printJSON(out, v)
	case "import":
		return importDocs(ctx, c, *dir, *glob, *apply, out)
	default:
		return errors.New("usage: doc put|get|list|import")
	}
}
func importDocs(ctx context.Context, c *remote.Client, dir, glob string, apply bool, out io.Writer) error {
	if dir == "" {
		return errors.New("doc import requires --dir")
	}
	n, rejected := 0, []map[string]string{}
	e := filepath.WalkDir(dir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		key, _ := filepath.Rel(dir, path)
		key = filepath.ToSlash(key)
		if glob != "**/*.md" {
			ok, _ := filepath.Match(glob, key)
			if !ok {
				return nil
			}
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		kind, job := importKind(key)
		if !apply {
			n++
			return nil
		}
		_, _, e = c.PutDocument(ctx, "", store.Document{Key: key, Kind: kind, Job: job, Body: string(b)})
		if e != nil {
			if p, ok := strings.CutPrefix(e.Error(), "secret_like_content:"); ok {
				rejected = append(rejected, map[string]string{"key": key, "pattern": p})
				return nil
			}
			return e
		}
		n++
		return nil
	})
	if e != nil {
		return e
	}
	return printJSON(out, map[string]any{"dry_run": !apply, "matched": n, "rejected": rejected})
}
func importKind(key string) (string, string) {
	p := strings.Split(key, "/")
	job := ""
	if len(p) >= 3 && p[0] == "jobs" {
		job = p[1]
	}
	base := filepath.Base(key)
	if base == "brief.md" {
		return "brief", job
	}
	if base == "report.md" {
		return "report", job
	}
	if strings.HasPrefix(base, "answer-") {
		return "answer", job
	}
	return "note", job
}

func validListen(addr string, tailnet bool) error {
	host, _, e := net.SplitHostPort(addr)
	if e != nil {
		return e
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("listen address must be an IP address")
	}
	if tailnet {
		if ip.To4() == nil || ip.To4()[0] != 100 || ip.To4()[1] < 64 || ip.To4()[1] > 127 {
			return errors.New("--listen-tailnet accepts only 100.64.0.0/10")
		}
		return nil
	}
	if !ip.IsLoopback() {
		return errors.New("--listen accepts loopback only")
	}
	return nil
}
func serve(args []string, errout io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errout)
	listen := fs.String("listen", "127.0.0.1:8800", "loopback address")
	tail := fs.String("listen-tailnet", "", "optional tailnet address")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if e := validListen(*listen, false); e != nil {
		return e
	}
	if *tail != "" {
		if e := validListen(*tail, true); e != nil {
			return e
		}
	}
	dbURL, auth := os.Getenv("HANDOFFKEEP_DB_URL"), os.Getenv("HANDOFFKEEP_AUTH_FILE")
	if dbURL == "" || auth == "" {
		return errors.New("HANDOFFKEEP_DB_URL and HANDOFFKEEP_AUTH_FILE are required")
	}
	ctx := context.Background()
	st, e := store.Open(ctx, dbURL)
	if e != nil {
		return e
	}
	defer st.Close()
	tokens, e := api.LoadTokens(auth)
	if e != nil {
		return e
	}
	svc := api.Service{Store: st}
	mux := http.NewServeMux()
	mux.Handle("/mcp", hkmcp.HTTPHandler(svc, tokens))
	mux.Handle("/", api.Server{Service: svc, Tokens: tokens}.Handler())
	servers := []*http.Server{{Addr: *listen, Handler: mux}}
	if *tail != "" {
		servers = append(servers, &http.Server{Addr: *tail, Handler: mux})
	}
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) { errs <- s.ListenAndServe() }(s)
	}
	return <-errs
}
func runMCP() error {
	u, t := config()
	if u == "" || t == "" {
		return errors.New("mcp requires HANDOFFKEEP_URL and HANDOFFKEEP_TOKEN")
	}
	return hkmcp.New(remote.Client{URL: u, Token: t}, "stdio").Run(context.Background(), &mcp.StdioTransport{})
}
