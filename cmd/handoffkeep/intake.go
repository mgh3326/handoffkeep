package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// taskIntakeCmd puts one item into triage; a repeated (source, source_ref)
// prints the recorded task with created:false.
//
//	tasks intake --source <name> --source-ref <ref> --title <text> [--body <text> | --body-file <path>] [--project <name>] [--kind <kind>] [--priority <n>] [--lane <name>]
func taskIntakeCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks intake", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	source := fs.String("source", "", "producer of the item, for example dots or linear")
	sourceRef := fs.String("source-ref", "", "the producer's id for the item; a repeat returns the recorded task")
	title := fs.String("title", "", "task title")
	body := fs.String("body", "", "task body, stored as an hk document")
	bodyFile := fs.String("body-file", "", "file holding the task body")
	project := fs.String("project", "", "task project (optional; the triage gate requires one on the way out)")
	kind := fs.String("kind", "", "task kind (optional; one of "+strings.Join(store.TaskKinds(), ", ")+")")
	priority := fs.Int("priority", 0, "task priority (optional)")
	lane := fs.String("lane", "", "task lane (default "+store.IntakeLane+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("tasks intake takes flags only")
	}
	if *source == "" || *sourceRef == "" || *title == "" {
		return errors.New("tasks intake requires --source, --source-ref and --title")
	}
	if *body != "" && *bodyFile != "" {
		return errors.New("--body and --body-file are exclusive")
	}
	in := store.IntakeInput{Source: *source, SourceRef: *sourceRef, Title: *title, Body: *body, Kind: *kind, Priority: *priority, Lane: *lane}
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			return err
		}
		in.Body = string(b)
	}
	fs.Visit(func(item *flag.Flag) {
		if item.Name == "project" {
			in.Project = project
		}
	})
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	x, created, err := c.Intake(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"created": created, "task": x})
}

// taskTriageCmd prints the triage queue, oldest first, with each ticket set
// as one row.
//
//	tasks triage [--limit <n>]
func taskTriageCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("tasks triage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	limit := fs.Int("limit", 100, "maximum rows (1-1000)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("tasks triage takes flags only")
	}
	if *limit < 1 || *limit > 1000 {
		return errors.New("--limit must be between 1 and 1000")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := c.ListTriage(ctx, *limit)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"triage": rows})
}

// ticketsCmd handles ticket sets.
//
//	tickets emit --file <json>
func ticketsCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "emit" {
		return errors.New("usage: tickets emit --file <json>")
	}
	fs := flag.NewFlagSet("tickets emit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	file := fs.String("file", "", "ticket set JSON document (docs/linear-internalize.md, Ticket sets); - reads stdin")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *file == "" {
		return errors.New("usage: tickets emit --file <json>")
	}
	var raw []byte
	var err error
	if *file == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(*file)
	}
	if err != nil {
		return err
	}
	// The same strict decoding as the server, so a typo in a key fails here
	// instead of being sent.
	var in store.TicketSetInput
	de := json.NewDecoder(bytes.NewReader(raw))
	de.DisallowUnknownFields()
	if err := de.Decode(&in); err != nil {
		return fmt.Errorf("tickets emit: %s: %w", *file, err)
	}
	if de.More() {
		return fmt.Errorf("tickets emit: %s: trailing data after the JSON document", *file)
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	set, created, err := c.EmitTicketSet(ctx, in)
	if err != nil {
		return err
	}
	return printJSON(out, struct {
		store.TicketSet
		Created bool `json:"created"`
	}{set, created})
}
