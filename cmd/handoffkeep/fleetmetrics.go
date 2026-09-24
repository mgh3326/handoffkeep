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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/fleetmetrics"
)

const fleetMetricsUsage = "usage: handoffkeep fleet-metrics --machine ID [--since 7d|RFC3339] [--until RFC3339] [--jobs-root PATH[=MACHINE]]... [--reps-file PATH[=SOURCE]]... [--no-reps-local] [--hosts-config PATH] [--slots MACHINE=N[@RFC3339]]... [--no-github] [--no-herdr] [--snapshot-out FILE] [--from-snapshot FILE] [--format md|json]"

// fleetMetricsCmd serves "fleet-metrics" (#644): a read-only daily report of
// the five Q6 operating metrics. Every source is read with GET requests or
// local read commands; nothing is written except --snapshot-out.
func fleetMetricsCmd(args []string, out io.Writer) error {
	return fleetMetricsRun(args, out, time.Now(), execRunner)
}

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return stdout.Bytes(), err
}

type multiFlag []string

func (m *multiFlag) String() string         { return strings.Join(*m, ",") }
func (m *multiFlag) Set(value string) error { *m = append(*m, value); return nil }

// parseSince accepts "<n>d", "<n>h" (and any Go duration) or an RFC3339 time.
func parseSince(v string, until time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || n < 1 {
			return time.Time{}, errors.New("--since: bad day count")
		}
		return until.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return time.Time{}, errors.New("--since takes 7d, 36h or an RFC3339 time")
	}
	return until.Add(-d), nil
}

func fleetMetricsRun(args []string, out io.Writer, now time.Time, run fleetmetrics.Runner) error {
	fs := flag.NewFlagSet("fleet-metrics", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := remoteClient(fs)
	since := fs.String("since", "7d", "window start: 7d, 36h or RFC3339")
	until := fs.String("until", "", "window end (RFC3339, default now)")
	machine := fs.String("machine", "", "hub machine id of this machine (owner of the default job root and [local] slots)")
	var jobRoots, repsFiles, slotFlags multiFlag
	fs.Var(&jobRoots, "jobs-root", "job directory root PATH[=MACHINE], repeatable (default ~/work/herdr-inbox/jobs=<machine>)")
	fs.Var(&repsFiles, "reps-file", "saved `scopefuel reps list` output PATH[=SOURCE], repeatable")
	fs.Var(&slotFlags, "slots", "task-slot capacity MACHINE=N or MACHINE=N@RFC3339 (dated timeline), repeatable; replaces hosts.toml for that machine")
	noRepsLocal := fs.Bool("no-reps-local", false, "do not run scopefuel reps list on this machine")
	home, _ := os.UserHomeDir()
	hostsConfig := fs.String("hosts-config", filepath.Join(home, ".config/wrk/hosts.toml"), "wrk hosts.toml for slot capacity ('' to skip)")
	noGitHub := fs.Bool("no-github", false, "do not read PR facts from GitHub (gh api)")
	noHerdr := fs.Bool("no-herdr", false, "do not read live panes (herdr agent list)")
	snapshotOut := fs.String("snapshot-out", "", "write the collected snapshot JSON here")
	fromSnapshot := fs.String("from-snapshot", "", "compute from a saved snapshot instead of live sources")
	format := fs.String("format", "md", "md or json")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w\n%s", err, fleetMetricsUsage)
	}
	if fs.NArg() != 0 {
		return errors.New(fleetMetricsUsage)
	}
	if *format != "md" && *format != "json" {
		return errors.New("--format must be md or json")
	}
	var snap fleetmetrics.Snapshot
	if *fromSnapshot != "" {
		b, err := os.ReadFile(*fromSnapshot)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &snap); err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		if snap.Schema != fleetmetrics.SnapshotSchema {
			return fmt.Errorf("snapshot schema %q, want %q", snap.Schema, fleetmetrics.SnapshotSchema)
		}
	} else {
		if *machine == "" {
			return errors.New("--machine is required (hub machine id, e.g. mac-personal)")
		}
		end := now
		if *until != "" {
			t, err := time.Parse(time.RFC3339, *until)
			if err != nil {
				return errors.New("--until takes RFC3339")
			}
			end = t
		}
		start, err := parseSince(*since, end)
		if err != nil {
			return err
		}
		if !start.Before(end) {
			return errors.New("--since must be before --until")
		}
		var overrides []fleetmetrics.SlotCapacity
		for _, spec := range slotFlags {
			m, rest, ok := strings.Cut(spec, "=")
			n, from, dated := strings.Cut(rest, "@")
			slots, err := strconv.Atoi(n)
			if !ok || m == "" || err != nil || slots < 0 {
				return errors.New("--slots takes MACHINE=N or MACHINE=N@RFC3339")
			}
			sc := fleetmetrics.SlotCapacity{Machine: m, Slots: slots, Source: "--slots " + spec}
			if dated {
				t, err := time.Parse(time.RFC3339, from)
				if err != nil {
					return errors.New("--slots MACHINE=N@TIME takes an RFC3339 time")
				}
				t = t.UTC()
				sc.From = &t
			}
			overrides = append(overrides, sc)
		}
		if err := mustClient(c); err != nil {
			return err
		}
		cfg := fleetmetrics.CollectConfig{HK: c, Run: run, Since: start, Until: end, Now: now, LocalMachine: *machine, RepsLocal: !*noRepsLocal, RepsFiles: repsFiles, GitHub: !*noGitHub, Herdr: !*noHerdr}
		cfg.SlotOverrides = overrides
		if len(jobRoots) == 0 {
			jobRoots = multiFlag{filepath.Join(home, "work/herdr-inbox/jobs") + "=" + *machine}
		}
		for _, spec := range jobRoots {
			path, m, ok := strings.Cut(spec, "=")
			if !ok {
				m = *machine
			}
			cfg.JobRoots = append(cfg.JobRoots, fleetmetrics.JobRoot{Path: path, Machine: m})
		}
		if *hostsConfig != "" {
			if _, err := os.Stat(*hostsConfig); err == nil {
				cfg.HostsConfig = *hostsConfig
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		snap, err = fleetmetrics.Collect(ctx, cfg)
		if err != nil {
			return err
		}
		if *snapshotOut != "" {
			b, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			if err := os.WriteFile(*snapshotOut, b, 0o600); err != nil {
				return err
			}
		}
	}
	report := fleetmetrics.Compute(snap)
	if *format == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", " ")
		return enc.Encode(report)
	}
	fleetmetrics.RenderMarkdown(out, report)
	return nil
}
