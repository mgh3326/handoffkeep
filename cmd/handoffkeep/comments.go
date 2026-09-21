package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// taskCommentsCmd serves "tasks comment <id> --body|--file" and
// "tasks comments <id>". There is no author flag: the server takes the author
// from the bearer token.
func taskCommentsCmd(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("tasks "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	body := fs.String("body", "", "comment body")
	file := fs.String("file", "", "comment body file (- reads stdin)")
	afterID := fs.Int64("after-id", 0, "list comments with id greater than this")
	limit := fs.Int("limit", 100, "maximum results")
	valueFlags := map[string]bool{"--url": true, "--token": true, "--body": true, "--file": true, "--after-id": true, "--limit": true}
	flags, positional := []string{}, []string{}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if valueFlags[arg] {
			i++
			if i >= len(rest) {
				return fmt.Errorf("%s requires a value", arg)
			}
			flags = append(flags, rest[i])
		}
	}
	if err := fs.Parse(append(flags, positional...)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: tasks %s <id>", args[0])
	}
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id < 1 {
		return errors.New("task id must be positive")
	}
	set := map[string]bool{}
	fs.Visit(func(item *flag.Flag) { set[item.Name] = true })
	switch args[0] {
	case "comment":
		if set["after-id"] || set["limit"] {
			return errors.New("tasks comment takes --body or --file")
		}
		if set["body"] == set["file"] {
			return errors.New("tasks comment requires exactly one of --body or --file")
		}
		text := *body
		if set["file"] {
			var b []byte
			if *file == "-" {
				b, err = io.ReadAll(io.LimitReader(in, store.TaskCommentMaxBytes+1))
			} else {
				b, err = os.ReadFile(*file)
			}
			if err != nil {
				return err
			}
			text = string(b)
		}
		if err := store.ValidateTaskCommentBody(text); err != nil {
			return err
		}
		if err := mustClient(c); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		x, err := c.CreateTaskComment(ctx, id, text)
		if err != nil {
			return err
		}
		return printJSON(out, x)
	default:
		if set["body"] || set["file"] {
			return errors.New("tasks comments takes --after-id and --limit")
		}
		if *afterID < 0 || *limit < 1 || *limit > store.TaskCommentListMax {
			return fmt.Errorf("--after-id must be >= 0 and --limit between 1 and %d", store.TaskCommentListMax)
		}
		if err := mustClient(c); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		xs, err := c.ListTaskComments(ctx, id, *afterID, *limit)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]any{"comments": xs})
	}
}
