package store

import "unicode/utf8"

// A read that keeps only the documents whose key starts with a prefix used to
// say so as `strpos(key,$1)=1` or `key LIKE $2 || '%' ESCAPE '\'`. On the
// en_US.UTF-8 database neither can use an index: strpos() is opaque to the
// planner, and the LIKE-to-range rewrite needs a constant pattern while these
// prefixes arrive as parameters. Every such read scanned the whole documents
// table, 171,534 times in twelve days (hk 4923).
//
// The index that serves them is documents_key_pattern, a btree on the key with
// the text_pattern_ops operator class: it compares bytes, so a prefix is the
// half-open range keyPrefixBounds returns, and a plain range scan serves a
// bound supplied as a parameter. The comparison operators of that opclass are
// spelled key ~>=~ lo AND key ~<~ hi — the same pair the planner derives from
// a prefix LIKE, written out because a parameterized pattern never becomes one.
//
// A range under the database collation is not an option: en_US.UTF-8 orders
// punctuation between letters and digits ("doc0" sorts after "doc/"), so the
// same bounds would both admit keys that do not start with the prefix and miss
// keys that do.

// keyPrefixSentinel is the largest code point, U+10FFFF. It is used only by a
// prefix whose own last code point is U+10FFFF, which has no successor.
const keyPrefixSentinel = "\U0010FFFF"

// keyPrefixBounds returns the bounds of the byte-wise range whose members are
// exactly the keys that start with prefix:
//
//	key ~>=~ lo AND key ~<~ hi
//
// Byte order for UTF-8 is code point order, so hi is the first string that
// sorts after every key beginning with prefix: the prefix without its last
// code point, followed by that code point's successor. Surrogates are not code
// points, so the successor of U+D7FF is U+E000.
//
// A prefix ending in U+10FFFF has no successor; its bound is then the prefix
// followed by U+10FFFF, which still admits every key it can be extended with
// except one whose next code point is U+10FFFF itself. No document key holds
// that character (keys are written as readable paths: deploy/, reports/,
// brief/, policy/…), so the bound is exact for every key the table holds.
//
// An empty prefix matches every key and never reaches this function: callers
// drop the predicate instead.
func keyPrefixBounds(prefix string) (lo, hi string) {
	r, size := utf8.DecodeLastRuneInString(prefix)
	if size <= 1 && r == utf8.RuneError {
		// Not valid UTF-8 (a Go caller's bug, or an empty prefix): no narrow
		// range is provable, so keep the sentinel bound, which admits every key.
		return prefix, prefix + keyPrefixSentinel
	}
	if r == utf8.MaxRune {
		return prefix, prefix + keyPrefixSentinel
	}
	next := r + 1
	if next == 0xD800 { // the surrogate block holds no code point
		next = 0xE000
	}
	return prefix, prefix[:len(prefix)-size] + string(next)
}
