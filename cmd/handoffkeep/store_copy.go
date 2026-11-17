package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const storeCopyExitMismatch = 2

func storeCopyCmd(args []string, out io.Writer) error {
	args, err := splitArgs(args, "from", "to", "tables")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("store-copy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "source database URL (postgres:// or mysql://)")
	to := fs.String("to", "", "destination URL; MySQL requires the copy admin account, PostgreSQL requires replica-role permission")
	tablesArg := fs.String("tables", "", "comma-separated registered tables; default is all ported tables")
	verifyOnly := fs.Bool("verify-only", false, "compare parity without copying")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *from == "" || *to == "" {
		return errors.New("--from and --to are required")
	}
	var tables []string
	if strings.TrimSpace(*tablesArg) != "" {
		tables = strings.Split(*tablesArg, ",")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	report, err := store.CopyStore(ctx, *from, *to, tables, *verifyOnly)
	if err != nil {
		return err
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if !report.Match {
		return exitCodeError{code: storeCopyExitMismatch, err: errors.New("store-copy parity mismatch; see per-table row counts and checksums")}
	}
	return nil
}
