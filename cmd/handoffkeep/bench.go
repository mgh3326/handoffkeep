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
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const benchAreaExceptionsUsage = "usage: handoffkeep bench area-exceptions list [--area A] [--include-retired] | put --file F"

// benchCmd serves the bench canon routes; today that is the area exception
// list only. list is open to any credential; put carries the operator
// credential and fails closed with the server's own operator_required for
// anyone else.
func benchCmd(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "area-exceptions" {
		return errors.New(benchAreaExceptionsUsage)
	}
	if len(args) < 2 || (args[1] != "list" && args[1] != "put") {
		return errors.New(benchAreaExceptionsUsage)
	}
	switch args[1] {
	case "list":
		return benchAreaExceptionsListCmd(args[2:], out)
	default:
		return benchAreaExceptionsPutCmd(args[2:], out)
	}
}

func benchAreaExceptionsListCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("bench area-exceptions list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	area := fs.String("area", "", "filter to one task area")
	includeRetired := fs.Bool("include-retired", false, "include retired exception rows")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(benchAreaExceptionsUsage)
	}
	if *area != "" {
		if err := store.ValidateTaskArea(area); err != nil {
			return err
		}
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	xs, err := c.ListBenchAreaExceptions(ctx, *area, *includeRetired)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"exceptions": xs})
}

func benchAreaExceptionsPutCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("bench area-exceptions put", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	file := fs.String("file", "", "JSON file holding {\"exceptions\":[...]}")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *file == "" {
		return errors.New(benchAreaExceptionsUsage)
	}
	body, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var input struct {
		Exceptions []store.BenchAreaException `json:"exceptions"`
	}
	de := json.NewDecoder(bytes.NewReader(body))
	de.DisallowUnknownFields()
	if err := de.Decode(&input); err != nil {
		return fmt.Errorf("invalid exceptions file: %w", err)
	}
	if len(input.Exceptions) < 1 {
		return errors.New("exceptions file carries an empty batch")
	}
	if err := mustClient(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := c.PutBenchAreaExceptions(ctx, input.Exceptions)
	if err != nil {
		return err
	}
	return printJSON(out, map[string]any{"upserted": n})
}
