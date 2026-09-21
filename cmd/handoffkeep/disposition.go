package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const dispositionUsage = "usage: tasks disposition add|summary|apply"

var (
	ghMergeSHARE = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ghPRURLRE    = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/pull/([0-9]+)$`)
)

// ghPRView is the subset of `gh pr view --json url,state,mergeCommit,mergedAt`
// the disposition facts are copied from. The CLI never runs gh itself.
type ghPRView struct {
	URL         string `json:"url"`
	State       string `json:"state"`
	MergeCommit *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
	MergedAt *time.Time `json:"mergedAt"`
}

func readGHPRView(path, originPR string) (string, *time.Time, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var view ghPRView
	if err := json.Unmarshal(b, &view); err != nil {
		return "", nil, fmt.Errorf("invalid --gh-json: %w", err)
	}
	if view.URL != originPR {
		return "", nil, errors.New("--gh-json url does not match --origin-pr")
	}
	if view.State != "MERGED" {
		return "", nil, errors.New("--gh-json state is not MERGED")
	}
	if view.MergeCommit == nil || !ghMergeSHARE.MatchString(view.MergeCommit.Oid) || view.MergedAt == nil {
		return "", nil, errors.New("--gh-json lacks a merge commit sha or mergedAt")
	}
	return view.MergeCommit.Oid, view.MergedAt, nil
}

// readResiduals accepts a JSON array of strings or objects with a title. The
// residual count is the array length; it is never typed by hand.
func readResiduals(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("invalid --residuals: %w", err)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		var text string
		if json.Unmarshal(item, &text) != nil {
			var object struct {
				Title    string `json:"title"`
				Severity string `json:"severity"`
				Evidence string `json:"evidence"`
			}
			if err := json.Unmarshal(item, &object); err != nil {
				return nil, fmt.Errorf("invalid --residuals item: %w", err)
			}
			text = object.Title
			if object.Severity != "" {
				text = "[" + object.Severity + "] " + text
			}
			if object.Evidence != "" {
				text += " — " + object.Evidence
			}
		}
		if strings.TrimSpace(text) == "" || strings.ContainsAny(text, "\r\n") {
			return nil, errors.New("invalid --residuals item: empty or multi-line")
		}
		out = append(out, text)
	}
	return out, nil
}

func dispositionOriginSlug(originPR string, originTask int64) string {
	if m := ghPRURLRE.FindStringSubmatch(originPR); m != nil {
		return m[1] + "-" + m[2] + "-pr" + m[3]
	}
	return "task" + strconv.FormatInt(originTask, 10)
}

func dispositionCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(dispositionUsage)
	}
	fs := flag.NewFlagSet("tasks disposition "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	lane := fs.String("lane", "", "director lane that applies the answer")
	title := fs.String("title", "", "item title (default: [처분] <origin>)")
	originPR := fs.String("origin-pr", "", "merged pull request URL")
	ghJSON := fs.String("gh-json", "", "file with gh pr view --json url,state,mergeCommit,mergedAt output")
	originTask := fs.Int64("origin-task", 0, "terminal parent task id")
	residuals := fs.String("residuals", "", "JSON array of residual items")
	recommended := fs.String("recommended", "", "recommended option A..E")
	note := fs.String("note", "", "short director note")
	installState := fs.String("install-state", "unknown", "unknown|not_installed|partial|installed|not_applicable")
	installPass := fs.Int("install-pass", 0, "targets with a passing probe")
	installTotal := fs.Int("install-total", 0, "enumerated targets")
	installWitness := fs.String("install-witness", "", "hk doc key of the install/probe witness")
	asOf := fs.String("as-of", "", "RFC3339 instant to reconstruct the summary at")
	asJSON := fs.Bool("json", false, "print the full summary as JSON")
	flags, positional := []string{}, []string{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if f := fs.Lookup(name); f != nil && !strings.Contains(arg, "=") {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
				continue
			}
			i++
			if i >= len(args) {
				return fmt.Errorf("%s requires a value", arg)
			}
			flags = append(flags, args[i])
		}
	}
	flags = append(flags, positional...)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "add":
		if fs.NArg() != 0 {
			return errors.New("tasks disposition add takes flags only")
		}
		if (*originPR == "") == (*originTask == 0) {
			return errors.New("exactly one of --origin-pr or --origin-task is required")
		}
		input := store.DispositionInput{Lane: *lane, Title: *title, OriginPR: *originPR, OriginTask: *originTask, Recommended: *recommended, Note: *note,
			Install: store.DispositionInstall{State: *installState, TargetsPass: *installPass, TargetsTotal: *installTotal, Witness: *installWitness}}
		if *originPR != "" {
			if *ghJSON == "" {
				return errors.New("--origin-pr requires --gh-json")
			}
			sha, mergedAt, err := readGHPRView(*ghJSON, *originPR)
			if err != nil {
				return err
			}
			input.MergeSHA, input.MergedAt = sha, mergedAt
		} else if *ghJSON != "" {
			return errors.New("--gh-json applies only to --origin-pr")
		}
		if input.Title == "" {
			if *originPR != "" {
				input.Title = "[처분] " + *originPR
			} else {
				input.Title = "[처분] #" + strconv.FormatInt(*originTask, 10)
			}
		}
		items, err := readResiduals(*residuals)
		if err != nil {
			return err
		}
		input.ResidualN = len(items)
		if len(items) > 0 {
			now := time.Now().UTC()
			key := fmt.Sprintf("disposition/%s/%s-%d", now.Format("2006-01-02"), dispositionOriginSlug(*originPR, *originTask), now.Unix())
			var body strings.Builder
			fmt.Fprintf(&body, "# 처분 잔여 — %s\n\n", input.Title)
			for i, item := range items {
				fmt.Fprintf(&body, "%d. %s\n", i+1, item)
			}
			if _, _, err := c.PutDocument(ctx, "", store.Document{Key: key, Kind: "note", Body: body.String()}); err != nil {
				return err
			}
			input.ResidualDoc = key
		}
		x, created, err := c.CreateDisposition(ctx, input)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"task": x, "created": created, "residual_doc": input.ResidualDoc})
	case "summary":
		if fs.NArg() != 0 {
			return errors.New("tasks disposition summary takes flags only")
		}
		if *asOf != "" {
			if _, err := time.Parse(time.RFC3339, *asOf); err != nil {
				return errors.New("--as-of must be RFC3339")
			}
		}
		summary, err := c.DispositionSummary(ctx, *asOf)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(out, summary)
		}
		_, err = fmt.Fprintf(out, "%s\n%s\n", summary.Line, summary.Detail)
		return err
	case "apply":
		if fs.NArg() != 1 {
			return errors.New("tasks disposition apply requires id")
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil || id < 1 {
			return errors.New("task id must be positive")
		}
		x, err := c.ApplyDisposition(ctx, id, *note)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	default:
		return errors.New(dispositionUsage)
	}
}
