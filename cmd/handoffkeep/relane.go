package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// relaneUsage covers every accepted form: one positional id, --ids a,b,c, or
// --ids - to read whitespace-separated ids from stdin.
const relaneUsage = "usage: tasks relane [<id>] --to <lane> --note <reason> [--ids a,b|-] [--allow-new-lane]"

// taskRelaneCmd serves "tasks relane". There is no actor flag: the server
// takes the event's "by" from the bearer token identity.
func taskRelaneCmd(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("tasks relane", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	to := fs.String("to", "", "target lane")
	note := fs.String("note", "", "relane reason recorded on the event")
	allowNewLane := fs.Bool("allow-new-lane", false, "permit a lane no current row uses")
	var idsFlag refFlags
	fs.Var(&idsFlag, "ids", "comma-separated task ids, repeatable; '-' reads whitespace-separated ids from stdin")
	valueFlags := map[string]bool{"--url": true, "--token": true, "--to": true, "--note": true, "--ids": true}
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
	if fs.NArg() > 1 {
		return errors.New(relaneUsage)
	}
	ids := []int64{}
	appendID := func(text string) error {
		id, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || id < 1 {
			return fmt.Errorf("task id %q must be positive", text)
		}
		ids = append(ids, id)
		return nil
	}
	if fs.NArg() == 1 {
		if err := appendID(fs.Arg(0)); err != nil {
			return err
		}
	}
	for _, group := range idsFlag {
		if strings.TrimSpace(group) == "-" {
			b, err := io.ReadAll(io.LimitReader(in, store.MaxBytes+1))
			if err != nil {
				return err
			}
			if len(b) > store.MaxBytes {
				return fmt.Errorf("stdin ids exceed %d bytes", store.MaxBytes)
			}
			for _, piece := range strings.Fields(string(b)) {
				if err := appendID(piece); err != nil {
					return err
				}
			}
			continue
		}
		for _, piece := range strings.Split(group, ",") {
			if err := appendID(piece); err != nil {
				return err
			}
		}
	}
	if len(ids) == 0 {
		return errors.New("tasks relane requires an id or --ids")
	}
	if len(ids) > api.TaskRelaneBatchMax {
		return fmt.Errorf("tasks relane accepts at most %d ids per request", api.TaskRelaneBatchMax)
	}
	if strings.TrimSpace(*to) == "" {
		return errors.New("tasks relane requires --to")
	}
	if strings.TrimSpace(*note) == "" {
		return errors.New("tasks relane requires --note")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	// Each item is its own server-side transaction; a max-size batch needs
	// longer than the default deadline, so the timeout scales with id count.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second+time.Duration(len(ids))*100*time.Millisecond)
	defer cancel()
	batch, err := c.RelaneTasks(ctx, ids, *to, *note, *allowNewLane)
	if err != nil {
		return err
	}
	// Results print before the summary error so a partial batch still leaves
	// its per-item evidence on stdout.
	if err := printJSON(out, batch); err != nil {
		return err
	}
	if batch.Failed > 0 || len(batch.Results) != len(ids) {
		return fmt.Errorf("relane failed for %d of %d task(s)", batch.Failed, len(ids))
	}
	return nil
}
