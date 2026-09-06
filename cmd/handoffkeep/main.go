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
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/attachments"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	hkmcp "github.com/mgh3326/handoffkeep/internal/mcp"
	"github.com/mgh3326/handoffkeep/internal/remote"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "handoffkeep:", err)
		var exit exitCodeError
		if errors.As(err, &exit) {
			os.Exit(exit.code)
		}
		os.Exit(1)
	}
}
func run(args []string, out, errout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: handoffkeep serve|mcp|ctx|memory|doc|attach|r2usage|tasks")
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
	case "attach":
		return attachCmd(args[1:], out)
	case "r2usage":
		return r2UsageCmd(args[1:], out)
	case "tasks":
		return tasksCmd(args[1:], out)
	default:
		return errors.New("usage: handoffkeep serve|mcp|ctx|memory|doc|attach|r2usage|tasks")
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

type refFlags []string

func (r *refFlags) String() string         { return strings.Join(*r, ",") }
func (r *refFlags) Set(value string) error { *r = append(*r, value); return nil }

func checkpointRefs(values []string) (store.Refs, error) {
	refs := store.Refs{}
	for _, value := range values {
		key, ref, ok := strings.Cut(value, "=")
		if !ok || key == "" || ref == "" {
			return nil, errors.New("--ref must be k=v")
		}
		refs[key] = append(refs[key], ref)
	}
	return refs, nil
}

func taskRefs(pr, headSHA, reportPath, jobID string) (*store.TaskRefs, bool) {
	x := &store.TaskRefs{PR: pr, HeadSHA: headSHA, ReportPath: reportPath, JobID: jobID}
	return x, pr != "" || headSHA != "" || reportPath != "" || jobID != ""
}

func normalizeTaskArgs(args []string) ([]string, error) {
	valueFlags := map[string]bool{"--url": true, "--token": true, "--lane": true, "--parent-lane": true, "--state": true, "--title": true, "--kind": true, "--priority": true, "--by": true, "--to": true, "--note": true, "--question": true, "--limit": true, "--pr": true, "--head-sha": true, "--report-path": true, "--job-id": true}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if valueFlags[arg] {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", arg)
				}
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...), nil
}

func tasksCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: tasks add|list|claim|next|transition|show")
	}
	fs := flag.NewFlagSet("tasks "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	lane := fs.String("lane", "", "task lane")
	parentLane := fs.String("parent-lane", "", "parent lane")
	state := fs.String("state", "", "task state")
	title := fs.String("title", "", "task title")
	kind := fs.String("kind", "implement", "task kind")
	priority := fs.Int("priority", 0, "higher is first")
	by := fs.String("by", "", "session label")
	to := fs.String("to", "", "target state")
	note := fs.String("note", "", "transition note")
	question := fs.String("question", "", "question required for needs_decision")
	limit := fs.Int("limit", 20, "maximum results")
	pr := fs.String("pr", "", "pull request reference")
	headSHA := fs.String("head-sha", "", "head revision reference")
	reportPath := fs.String("report-path", "", "report path reference")
	jobID := fs.String("job-id", "", "job reference")
	parseArgs, err := normalizeTaskArgs(args[1:])
	if err != nil {
		return err
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	parseID := func() (int64, error) {
		if fs.NArg() != 1 {
			return 0, errors.New("task command requires id")
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return 0, errors.New("task id must be positive")
		}
		return id, nil
	}
	switch args[0] {
	case "add":
		if fs.NArg() != 0 {
			return errors.New("tasks add takes flags only")
		}
		refs, _ := taskRefs(*pr, *headSHA, *reportPath, *jobID)
		x, err := c.CreateTask(ctx, store.Task{Lane: *lane, ParentLane: *parentLane, Title: *title, Kind: *kind, Priority: *priority, Refs: *refs})
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case "list":
		if fs.NArg() != 0 {
			return errors.New("tasks list takes flags only")
		}
		xs, err := c.ListTasks(ctx, *lane, *state, *parentLane, *limit)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"tasks": xs})
	case "claim":
		id, err := parseID()
		if err != nil {
			return err
		}
		x, err := c.ClaimTask(ctx, id, *by)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case "next":
		if fs.NArg() != 0 {
			return errors.New("tasks next takes flags only")
		}
		x, err := c.NextTask(ctx, *lane, *by)
		if err != nil {
			if err.Error() == "queue_empty" {
				return exitCodeError{code: 3, err: errors.New("no backlog task")}
			}
			return err
		}
		return printJSON(out, x)
	case "transition":
		id, err := parseID()
		if err != nil {
			return err
		}
		if *to == "needs_decision" {
			if strings.TrimSpace(*question) == "" {
				return errors.New("tasks transition --to needs_decision requires --question")
			}
			if *note == "" {
				*note = *question
			}
		}
		refs, hasRefs := taskRefs(*pr, *headSHA, *reportPath, *jobID)
		if !hasRefs {
			refs = nil
		}
		x, err := c.TransitionTask(ctx, id, *to, *note, refs)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	case "show":
		id, err := parseID()
		if err != nil {
			return err
		}
		x, found, err := c.GetTask(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("not_found")
		}
		return printJSON(out, x)
	default:
		return errors.New("usage: tasks add|list|claim|next|transition|show")
	}
}

type exitCodeError struct {
	code int
	err  error
}

func (e exitCodeError) Error() string { return e.err.Error() }

// searchArgs permits the query before or after flags. The standard flag package
// stops parsing at a positional argument, so normalize its known value flags.
func searchArgs(args []string) ([]string, []string, error) {
	valueFlags := map[string]bool{"--url": true, "--token": true, "--session": true, "--limit": true, "--kind": true, "--scope": true}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if valueFlags[arg] {
				i++
				if i >= len(args) {
					return nil, nil, fmt.Errorf("%s requires a value", arg)
				}
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return flags, positional, nil
}

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
	var refs refFlags
	fs.Var(&refs, "ref", "checkpoint reference k=v (repeatable)")
	parseArgs := args[1:]
	var searchQuery []string
	if args[0] == "search" {
		var e error
		parseArgs, searchQuery, e = searchArgs(parseArgs)
		if e != nil {
			return e
		}
	}
	if e := fs.Parse(parseArgs); e != nil {
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
		checkpointRefs, e := checkpointRefs(refs)
		if e != nil {
			return e
		}
		v, e := c.Checkpoint(ctx, "", store.Checkpoint{Session: *session, Kind: *kind, Title: *title, Body: *body, Refs: checkpointRefs})
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
		if len(searchQuery) != 1 {
			return errors.New("ctx search requires query")
		}
		v, e := c.Search(ctx, searchQuery[0], *scope, *session, *limit)
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
		raw := x.Content
		if !strings.HasPrefix(raw, "---\n") {
			raw = fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n---\n%s", x.Name, x.Description, x.Type, x.Content)
		}
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

func attachCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: attach put|get|list|usage")
	}
	fs := flag.NewFlagSet("attach "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	checkpoint := fs.String("checkpoint", "", "checkpoint id")
	doc := fs.String("doc", "", "document key")
	memory := fs.String("memory", "", "memory agent/name")
	name := fs.String("name", "", "original filename")
	output := fs.String("o", "", "output path")
	// Permit the documented `attach get SHA -o file` form as well as flags first.
	valueFlags := map[string]bool{"--url": true, "--token": true, "--checkpoint": true, "--doc": true, "--memory": true, "--name": true, "-o": true}
	flags, positional := []string{}, []string{}
	for i := 1; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if valueFlags[a] {
				i++
				if i >= len(args) {
					return fmt.Errorf("%s requires a value", a)
				}
				flags = append(flags, args[i])
			}
		} else {
			positional = append(positional, a)
		}
	}
	if e := fs.Parse(append(flags, positional...)); e != nil {
		return e
	}
	if e := mustClient(c); e != nil {
		return e
	}
	ctx := context.Background()
	ref := ""
	n := 0
	if *checkpoint != "" {
		ref = "checkpoint:" + *checkpoint
		n++
	}
	if *doc != "" {
		ref = "document:" + *doc
		n++
	}
	if *memory != "" {
		ref = "memory:" + *memory
		n++
	}
	if n > 1 {
		return errors.New("only one attachment reference may be specified")
	}
	switch args[0] {
	case "put":
		if fs.NArg() != 1 {
			return errors.New("attach put requires a file")
		}
		p := fs.Arg(0)
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		if *name == "" {
			*name = filepath.Base(p)
		}
		contentType := attachmentMIME(*name)
		if contentType == "" {
			contentType = http.DetectContentType(b)
		}
		x, created, e := c.PutAttachment(ctx, "", *name, contentType, ref, b)
		if e != nil {
			return e
		}
		return printJSON(out, map[string]any{"sha256": x.SHA256, "size_bytes": x.SizeBytes, "url": "/v1/attachments/" + x.SHA256, "created": created})
	case "get":
		if fs.NArg() != 1 {
			return errors.New("attach get requires sha")
		}
		x, r, e := c.GetAttachment(ctx, fs.Arg(0))
		if e != nil {
			return e
		}
		defer r.Close()
		if *output == "" {
			_, e = io.Copy(out, r)
			return e
		}
		f, e := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		_, e = io.Copy(f, r)
		if e == nil {
			return printJSON(out, map[string]any{"sha256": x.SHA256, "output": *output})
		}
		return e
	case "list":
		xs, e := c.ListAttachments(ctx, ref, 100)
		if e != nil {
			return e
		}
		return printJSON(out, map[string]any{"attachments": xs})
	case "usage":
		u, e := c.AttachmentUsage(ctx)
		if e != nil {
			return e
		}
		return printJSON(out, u)
	default:
		return errors.New("usage: attach put|get|list|usage")
	}
}

func attachmentMIME(name string) string {
	return map[string]string{".json": "application/json", ".md": "text/markdown", ".csv": "text/csv", ".ndjson": "application/x-ndjson", ".log": "text/x-log", ".txt": "text/plain", ".html": "text/html", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".pdf": "application/pdf"}[strings.ToLower(filepath.Ext(name))]
}

// r2usage intentionally does no object-store access. It is safe to run where
// credentials are absent and reports a skipped result in that case.
func r2UsageCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("r2usage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	alert := fs.Bool("alert", false, "send threshold alert")
	if e := fs.Parse(args); e != nil {
		return e
	}
	token, account := os.Getenv("CF_API_TOKEN"), os.Getenv("CF_ACCOUNT_ID")
	if token == "" || account == "" {
		return printJSON(out, map[string]any{"skipped": true, "reason": "CF_API_TOKEN and CF_ACCOUNT_ID are required"})
	}
	query := `query($account:String!,$month:Time!){viewer{accounts(filter:{accountTag:$account}){r2StorageAdaptiveGroups(limit:1,orderBy:[datetime_DESC]){sum{payloadSize objectCount}} r2OperationsAdaptiveGroups(limit:100,filter:{datetime_geq:$month}){dimensions{actionType} sum{requests}}}}}`
	month := time.Now().UTC().Format("2006-01") + "-01"
	payload, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"account": account, "month": month}})
	req, e := http.NewRequest(http.MethodPost, "https://api.cloudflare.com/client/v4/graphql", strings.NewReader(string(payload)))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudflare GraphQL returned %s", resp.Status)
	}
	var result struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Storage []struct {
						Sum struct{ PayloadSize, ObjectCount int64 } `json:"sum"`
					} `json:"r2StorageAdaptiveGroups"`
					Operations []struct {
						Dimensions struct {
							ActionType string `json:"actionType"`
						} `json:"dimensions"`
						Sum struct {
							Requests int64 `json:"requests"`
						} `json:"sum"`
					} `json:"r2OperationsAdaptiveGroups"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []any `json:"errors"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&result); e != nil {
		return e
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("Cloudflare GraphQL returned errors")
	}
	var bytesTotal, objects, classA, classB int64
	if len(result.Data.Viewer.Accounts) > 0 {
		a := result.Data.Viewer.Accounts[0]
		if len(a.Storage) > 0 {
			bytesTotal = a.Storage[0].Sum.PayloadSize
			objects = a.Storage[0].Sum.ObjectCount
		}
		for _, op := range a.Operations {
			if strings.Contains(strings.ToLower(op.Dimensions.ActionType), "class a") {
				classA += op.Sum.Requests
			} else if strings.Contains(strings.ToLower(op.Dimensions.ActionType), "class b") {
				classB += op.Sum.Requests
			}
		}
	}
	alertReason := r2AlertReason(bytesTotal, classA, classB)
	output := map[string]any{"storage_bytes": bytesTotal, "object_count": objects, "class_a_month": classA, "class_b_month": classB, "storage_ratio": float64(bytesTotal) / float64(10<<30), "alert": alertReason != "", "alert_reason": alertReason}
	// Compare the independent Cloudflare total with the local immutable-object
	// ledger when this CLI has normal handoffkeep HTTP credentials configured.
	if localURL, localToken := config(); localURL != "" && localToken != "" {
		if local, err := (remote.Client{URL: localURL, Token: localToken}).AttachmentUsage(context.Background()); err == nil {
			output["local_bytes_total"] = local.TotalBytes
			base := bytesTotal
			if base == 0 {
				base = local.TotalBytes
			}
			diff := bytesTotal - local.TotalBytes
			if diff < 0 {
				diff = -diff
			}
			if base > 0 && diff*10 > base {
				alertReason = "R2 storage differs from local attachment ledger by more than 10%"
				output["alert"] = true
				output["alert_reason"] = alertReason
			}
		}
	}
	if *alert && alertReason != "" {
		line := "handoffkeep r2usage alert: " + alertReason
		if hook := os.Getenv("HK_ALERT_DISCORD_WEBHOOK"); hook != "" {
			b, _ := json.Marshal(map[string]string{"content": line})
			_, _ = http.Post(hook, "application/json", strings.NewReader(string(b)))
		} else {
			fmt.Fprintln(out, line)
		}
	}
	return printJSON(out, output)
}

func r2AlertReason(bytesTotal, classA, classB int64) string {
	const storageFree = int64(10 << 30)
	for _, x := range []struct {
		value, cap int64
		label      string
	}{
		{bytesTotal, storageFree, "R2 storage"}, {classA, 1000000, "R2 Class A operations"}, {classB, 10000000, "R2 Class B operations"},
	} {
		if x.value*100 >= 90*x.cap {
			return x.label + " are at least 90% of free tier"
		}
		if x.value*100 >= 70*x.cap {
			return x.label + " are at least 70% of free tier"
		}
	}
	return ""
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
	am, e := attachments.NewFromEnv(st)
	if e != nil {
		return e
	}
	svc := api.Service{Store: st, Attachments: am}
	uiHandler, e := uiFromEnv(st)
	if e != nil {
		return e
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", hkmcp.HTTPHandler(svc, tokens))
	mux.Handle("/", api.Server{Service: svc, Tokens: tokens, UI: uiHandler}.Handler())
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

// uiFromEnv intentionally treats incomplete Cloudflare Access configuration as
// disabled. The API remains available, but api.Server does not register /ui.
func uiFromEnv(st *store.Store) (http.Handler, error) {
	team := strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_CF_TEAM_DOMAIN"))
	aud := strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_CF_AUD"))
	emails := strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_ALLOWED_EMAILS"))
	if team == "" || aud == "" || emails == "" {
		return nil, nil
	}
	access, err := cfaccess.New(cfaccess.Config{
		TeamDomain:          team,
		AUD:                 aud,
		AllowedEmails:       strings.Split(emails, ","),
		AllowedServiceNames: strings.Split(strings.TrimSpace(os.Getenv("HANDOFFKEEP_UI_ALLOWED_SERVICE_NAMES")), ","),
	})
	if err != nil {
		return nil, err
	}
	return ui.New(ui.Config{
		Store:    st,
		Access:   access,
		HubURL:   strings.TrimSpace(os.Getenv("HANDOFFKEEP_HUB_URL")),
		HubToken: strings.TrimSpace(os.Getenv("HANDOFFKEEP_HUB_TOKEN")),
	})
}
func runMCP() error {
	u, t := config()
	if u == "" || t == "" {
		return errors.New("mcp requires HANDOFFKEEP_URL and HANDOFFKEEP_TOKEN")
	}
	return hkmcp.NewStdio(remote.Client{URL: u, Token: t}, "stdio").Run(context.Background(), &mcp.StdioTransport{})
}
