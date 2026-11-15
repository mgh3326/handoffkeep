package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/mgh3326/handoffkeep/client"
	"github.com/mgh3326/handoffkeep/internal/attachmentarchive"
)

// attachArchiveCmd copies attachments whose every referring task is closed
// into the Drive archive and records each copy with hk (design
// 2026-11-16/hk-attachment-archive). It is a dry run unless --apply is set.
func attachArchiveCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("attach archive", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", "", "Drive mount that holds the "+attachmentarchive.MarkerFile+" marker")
	apply := fs.Bool("apply", false, "copy, verify, write manifests and record; the default is a dry run")
	limit := fs.Int("limit", 0, "most candidates to process (0 = all)")
	rc := remoteClient(fs)
	if e := fs.Parse(args); e != nil {
		return e
	}
	if fs.NArg() != 0 {
		return errors.New("attach archive takes no positional arguments")
	}
	if *limit < 0 {
		return errors.New("--limit must not be negative")
	}
	if e := mustClient(rc); e != nil {
		return e
	}
	sum, e := attachmentarchive.Archive(context.Background(), client.Client(*rc), attachmentarchive.Options{
		Root:  *root,
		Apply: *apply,
		Limit: *limit,
	})
	if e != nil {
		return e
	}
	return printJSON(out, sum)
}
