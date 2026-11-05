package main

import (
	"fmt"
	"strings"
)

// splitArgs is the one way subcommands let flags and positionals come in any
// order: it moves flags (with their values) ahead of the positionals so the
// flag package sees them all. valueFlags names the flags that take a value,
// without dashes; --url and --token always do. A flag matches in its -x and
// --x spellings, and -x=v / --x=v carries its own value.
//
// A value flag never takes a following flag as its value: "--reason --block"
// is a missing value, not reason="--block" with --block silently dropped.
// Text that starts with "-" is passed in the explicit "--reason=-text" form.
func splitArgs(args []string, valueFlags ...string) ([]string, error) {
	takes := map[string]bool{"url": true, "token": true}
	for _, f := range valueFlags {
		takes[f] = true
	}
	flags, positional := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		if strings.Contains(arg, "=") || !takes[strings.TrimLeft(arg, "-")] {
			continue
		}
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("%s requires a value", arg)
		}
		if strings.HasPrefix(args[i], "-") && args[i] != "-" {
			return nil, fmt.Errorf("%s requires a value, got flag %q (write %s=%s for text that starts with -)", arg, args[i], arg, args[i])
		}
		flags = append(flags, args[i])
	}
	return append(flags, positional...), nil
}
