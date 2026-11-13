package main

import (
	"errors"
	"regexp"
	"strconv"
)

// docOlderThanArg is the --older-than form: a whole number of days, "30d".
var docOlderThanArg = regexp.MustCompile(`^([1-9][0-9]{0,4})d$`)

// parseDocOlderThan returns the day count of an --older-than value. It
// refuses anything but Nd, so a mistyped unit never prunes by accident.
func parseDocOlderThan(arg string) (int, error) {
	m := docOlderThanArg.FindStringSubmatch(arg)
	if m == nil {
		return 0, errors.New("doc prune --older-than takes a whole number of days, like 30d")
	}
	return strconv.Atoi(m[1])
}
