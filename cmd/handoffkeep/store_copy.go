package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const storeCopyExitMismatch = 2

// userinfoPattern matches the userinfo of a URL with a scheme ("scheme://user:pass@").
var userinfoPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]*@`)

func storeCopyCmd(args []string, out io.Writer) error {
	var urls []string
	err := storeCopyRun(args, out, &urls)
	return redactStoreCopyError(err, urls)
}

// storeCopyRun resolves both URLs into urls before connecting, so a failure at
// any point can be redacted with every URL the command read.
func storeCopyRun(args []string, out io.Writer, urls *[]string) error {
	args, err := splitArgs(args, "from", "from-env", "to", "to-env", "tables")
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("store-copy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "source database URL (postgres:// or mysql://); prefer --from-env")
	fromEnv := fs.String("from-env", "", "name of the environment variable holding the source URL")
	to := fs.String("to", "", "destination URL; MySQL sets @hk_bulk_load on the copy session; prefer --to-env")
	toEnv := fs.String("to-env", "", "name of the environment variable holding the destination URL")
	tablesArg := fs.String("tables", "", "comma-separated registered tables; default is all ported tables (tasks requires sync_decisions)")
	verifyOnly := fs.Bool("verify-only", false, "compare parity without copying")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	fromURL, err := storeCopyURL("from", *from, *fromEnv)
	if err != nil {
		return err
	}
	*urls = append(*urls, fromURL)
	toURL, err := storeCopyURL("to", *to, *toEnv)
	if err != nil {
		return err
	}
	*urls = append(*urls, toURL)
	var tables []string
	if strings.TrimSpace(*tablesArg) != "" {
		tables = strings.Split(*tablesArg, ",")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	report, err := store.CopyStore(ctx, fromURL, toURL, tables, *verifyOnly)
	if err != nil {
		return storeCopyFailure(out, report, err)
	}
	if err := printJSON(out, report); err != nil {
		return err
	}
	if !report.Match {
		return exitCodeError{code: storeCopyExitMismatch, err: errors.New("store-copy parity mismatch; see per-table row counts and checksums")}
	}
	return nil
}

// storeCopyURL returns the database URL named by --name, or read from the
// environment variable given by --name-env. Exactly one of the two is set. The
// errors name the flag or variable, never its value.
func storeCopyURL(name, value, envName string) (string, error) {
	if envName == "" {
		if value == "" {
			return "", fmt.Errorf("--%s or --%s-env is required", name, name)
		}
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("--%s and --%s-env are mutually exclusive", name, name)
	}
	got, ok := os.LookupEnv(envName)
	if !ok || got == "" {
		return "", fmt.Errorf("environment variable %s (for --%s-env) is unset or empty", envName, name)
	}
	return got, nil
}

// redactStoreCopyError removes the userinfo of every URL the command read, and
// any URL-looking userinfo at all, from an error before it reaches stderr.
func redactStoreCopyError(err error, urls []string) error {
	if err == nil {
		return nil
	}
	msg := redactDBURLs(err.Error(), urls)
	if msg == err.Error() {
		return err
	}
	var exit exitCodeError
	if errors.As(err, &exit) {
		return exitCodeError{code: exit.code, err: errors.New(msg)}
	}
	return errors.New(msg)
}

func redactDBURLs(text string, urls []string) string {
	text = userinfoPattern.ReplaceAllString(text, "${1}***@")
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			continue
		}
		// A bare password can be a short word that also occurs in ordinary
		// text, so only the ":password@" form of an echoed userinfo is replaced.
		if pw, ok := u.User.Password(); ok && pw != "" {
			text = strings.ReplaceAll(text, ":"+pw+"@", ":***@")
		}
	}
	return text
}

func storeCopyFailure(out io.Writer, report store.CopyReport, err error) error {
	if len(report.Tables) == 0 {
		return err
	}
	if printErr := printJSON(out, report); printErr != nil {
		return errors.Join(err, fmt.Errorf("print partial store-copy report: %w", printErr))
	}
	return err
}
