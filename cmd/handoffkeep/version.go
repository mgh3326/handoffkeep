package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
)

// readBuildInfo is a variable so tests can stamp a VCS-tagged build info,
// the same pattern internal/api uses for healthz and tasks export.
var readBuildInfo = debug.ReadBuildInfo

type versionFields struct {
	Rev      string `json:"rev"`
	Time     string `json:"time"`
	Modified string `json:"modified"`
	Module   string `json:"module"`
	Go       string `json:"go"`
}

// buildVersion reads the binary's embedded build stamp. Any field absent
// from the stamp — and every field when no build info exists — reports the
// literal "unknown"; a value is never empty, shortened, or derived from
// another field. In particular rev comes only from vcs.revision, never
// from the module version.
func buildVersion() versionFields {
	v := versionFields{Rev: "unknown", Time: "unknown", Modified: "unknown", Module: "unknown", Go: "unknown"}
	if info, ok := readBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Value == "" {
				continue
			}
			switch setting.Key {
			case "vcs.revision":
				v.Rev = setting.Value
			case "vcs.time":
				v.Time = setting.Value
			case "vcs.modified":
				v.Modified = setting.Value
			}
		}
		if info.Main.Version != "" {
			v.Module = info.Main.Version
		}
		if info.GoVersion != "" {
			v.Go = info.GoVersion
		}
	}
	return v
}

// versionCmd prints the embedded build stamp. It reads no environment or
// config and never contacts a server, so it works on a bare install.
func versionCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the version fields as one JSON object")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: handoffkeep version [--json]")
	}
	v := buildVersion()
	if *asJSON {
		return printJSON(out, v)
	}
	_, err := fmt.Fprintf(out, "handoffkeep rev=%s time=%s modified=%s module=%s go=%s\n", v.Rev, v.Time, v.Modified, v.Module, v.Go)
	return err
}
