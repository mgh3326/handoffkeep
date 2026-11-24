package store

import (
	"strings"
	"unicode/utf8"
)

// A read that keeps only the documents whose key starts with a prefix used to
// say so as `strpos(key,$1)=1` or `key LIKE $2 || '%' ESCAPE '\'`. On the
// en_US.UTF-8 database neither can use an index: strpos() is opaque to the
// planner, and the LIKE-to-range rewrite needs a constant pattern while these
// prefixes arrive as parameters. Every such read scanned the whole documents
// table, 171,534 times in twelve days (hk 4923).
//
// The index that serves them is documents_key_c, a btree on the key under the
// "C" collation, i.e. byte order: a prefix is the half-open range
// keyPrefixBounds returns, and the planner serves a bound supplied as a
// parameter with an index scan. The same collation serves the ORDER BY of the
// cursor-paged reads, so one ordering covers the bounds, the cursor and the
// sort, and a page costs a page of index rather than a sort of the whole
// prefix range.
//
// A range under the database collation is not an option: en_US.UTF-8 orders
// punctuation between letters and digits ("doc0" sorts after "doc/"), so the
// same bounds would both admit keys that do not start with the prefix and miss
// keys that do.

// keyPrefixSentinel is the largest code point, U+10FFFF.
const keyPrefixSentinel = "\U0010FFFF"

// maxDocKeyBytes is the key length limit ValidDocKey enforces. A prefix range
// is closed with it when the prefix's own last code point has no successor.
const maxDocKeyBytes = 512

// keyPrefixBounds returns the bounds of the byte-wise range whose members are
// exactly the keys that start with prefix:
//
//	key COLLATE "C" >= lo AND key COLLATE "C" < hi
//
// Byte order for UTF-8 is code point order, so hi is the first string that
// sorts after every key beginning with prefix. Usually that is the prefix
// without its last code point, followed by that code point's successor;
// surrogates are not code points, so the successor of U+D7FF is U+E000.
//
// A last code point of U+10FFFF has no successor, and no byte string above it
// is valid UTF-8. The bound is then built from the key length instead: no key
// is longer than maxDocKeyBytes, so the longest key that can start with prefix
// is the prefix followed by U+10FFFF up to that length, and one more U+10FFFF
// sorts above it without being a key itself. That keeps the range exact across
// a blue-green rollout too, while the previous release can still write a key
// it would not validate.
//
// A prefix that is not valid UTF-8 matches nothing (every key is valid UTF-8),
// and the same length bound admits no key for it. An empty prefix matches every
// key and never reaches this function: callers drop the predicate instead.
func keyPrefixBounds(prefix string) (lo, hi string) {
	r, size := utf8.DecodeLastRuneInString(prefix)
	if r != utf8.MaxRune && !(size <= 1 && r == utf8.RuneError) {
		next := r + 1
		if next == 0xD800 { // the surrogate block holds no code point
			next = 0xE000
		}
		return prefix, prefix[:len(prefix)-size] + string(next)
	}
	// One U+10FFFF past the longest key length: every key starting with prefix
	// sorts below it, and no key can equal it.
	run := maxDocKeyBytes - len(prefix) + 1
	if run < 1 {
		run = 1
	}
	return prefix, prefix + strings.Repeat(keyPrefixSentinel, run)
}
