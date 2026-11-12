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

// `youtrack authority` (hk 4122, docs/youtrack-write-protocol.md §Authority):
// which side is canonical per kind, and the switch. Both go through the
// server (/v1/yt/authority), because the write modes a switch checks are the
// server's.

const youtrackAuthorityUsage = "usage: youtrack authority show | youtrack authority set <tasks|comments|documents|decisions|all> <hk|youtrack> --by NAME --note TEXT"

func youtrackAuthorityCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(youtrackAuthorityUsage)
	}
	switch args[0] {
	case "show":
		return youtrackAuthorityShowCmd(args[1:], out)
	case "set":
		return youtrackAuthoritySetCmd(args[1:], out)
	default:
		return errors.New(youtrackAuthorityUsage)
	}
}

func youtrackAuthorityShowCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack authority show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	ordered, err := splitArgs(args, "url", "token")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(youtrackAuthorityUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := c.YTAuthorities(ctx)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"authority": rows})
}

func youtrackAuthoritySetCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("youtrack authority set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	by := fs.String("by", "", "operator who makes the switch")
	note := fs.String("note", "", "why, and what was checked")
	ordered, err := splitArgs(args, "by", "note", "url", "token")
	if err != nil {
		return err
	}
	if err := fs.Parse(ordered); err != nil {
		return err
	}
	if fs.NArg() != 2 || strings.TrimSpace(*by) == "" || strings.TrimSpace(*note) == "" {
		return errors.New(youtrackAuthorityUsage)
	}
	kinds, err := ytAuthorityKindsArg(fs.Arg(0))
	if err != nil {
		return err
	}
	side := store.YTAuthoritySide(fs.Arg(1))
	if side != store.YTAuthorityHK && side != store.YTAuthorityYouTrack {
		return fmt.Errorf("unknown authority %q; %s", fs.Arg(1), youtrackAuthorityUsage)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before, after, err := c.SetYTAuthority(ctx, kinds, side, *by, *note)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"before": before, "after": after})
}

// ytAuthorityKindsArg reads the kind argument: one kind, or all.
func ytAuthorityKindsArg(arg string) ([]store.YTAuthorityKind, error) {
	if arg == "all" {
		return store.YTAuthorityKinds, nil
	}
	for _, k := range store.YTAuthorityKinds {
		if string(k) == arg {
			return []store.YTAuthorityKind{k}, nil
		}
	}
	return nil, fmt.Errorf("unknown kind %q; %s", arg, youtrackAuthorityUsage)
}

// youtrackRefuseAuthority refuses a legacy push (sync, adopt) while any kind
// is canonical in YouTrack: the push copies PG over YouTrack. The protocol
// mode guard (youtrackRefuseLegacyPush) already refuses while a mode is on;
// this one also holds after a mode is turned off.
func youtrackRefuseAuthority(ctx context.Context, st *store.Store, sub string) error {
	rows, err := st.YTAuthorities(ctx)
	if err != nil {
		return err
	}
	var canonical []string
	for _, a := range rows {
		if a.Authority == store.YTAuthorityYouTrack {
			canonical = append(canonical, string(a.Kind))
		}
	}
	if len(canonical) == 0 {
		return nil
	}
	return exitCodeError{code: youtrackExitProtocolOwned, err: fmt.Errorf("youtrack %s refused: YouTrack is canonical for %s (youtrack authority show); a legacy push would copy PG over it (hk 4122)",
		sub, strings.Join(canonical, ", "))}
}
