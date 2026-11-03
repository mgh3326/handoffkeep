// Package buildinfo reads the binary's embedded Go build stamp.
package buildinfo

import "runtime/debug"

// read is a variable so tests can stamp a VCS-tagged build info.
var read = debug.ReadBuildInfo

// Revision is the vcs.revision the binary was built from, the same value
// `handoffkeep version` prints as rev, or "unknown" for an unstamped build
// (go test binaries, source exports, -buildvcs=false).
func Revision() string {
	if info, ok := read(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return s.Value
			}
		}
	}
	return "unknown"
}
