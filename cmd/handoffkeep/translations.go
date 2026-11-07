package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func translationsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "put" && args[0] != "missing") {
		return errors.New("usage: translations put --file FILE | translations missing --entity task --state open [--limit N]")
	}
	action := args[0]
	fs := flag.NewFlagSet("translations "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	file := fs.String("file", "", "JSON array of translations")
	entity := fs.String("entity", "task", "entity")
	state := fs.String("state", "open", "state")
	limit := fs.Int("limit", 100, "maximum results")
	parsed, err := splitArgs(args[1:], "file", "entity", "state", "limit")
	if err != nil {
		return err
	}
	if err = fs.Parse(parsed); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if err = mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if action == "put" {
		if *file == "" {
			return errors.New("translations put requires --file")
		}
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		var rows []store.Translation
		dec := json.NewDecoder(io.LimitReader(f, store.TranslationBatchMaxBytes+1))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&rows); err != nil {
			return err
		}
		var extra any
		if err = dec.Decode(&extra); err != io.EOF {
			return errors.New("trailing JSON in translation file")
		}
		n, err := c.PutTranslations(ctx, rows)
		if err != nil {
			return err
		}
		return printJSON(out, map[string]int{"count": n})
	}
	rows, err := c.MissingTranslations(ctx, *entity, *state, *limit)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"translations": rows})
}
