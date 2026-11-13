package youtrack

import "unicode/utf8"

// truncateRunes cuts s to n characters, as Python slices a str.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
