package ytwrite

import (
	"errors"
	"net"
)

// beforeSend reports whether a failed remote call provably happened before any
// part of the request was written, so sending it again cannot duplicate
// anything: name resolution failed, no connection was made (a dial error), or
// the request could not be built. Anything else — a timeout, a connection lost
// after the request was written, a 5xx — may have reached YouTrack, so the
// protocol records the op remote_unknown and leaves it to the evidence search
// (hk 4797: a family-root POST that timed out was retried as not_sent, and the
// retry made a second root beside the one the first POST had created).
//
// This is the one place the protocol decides that question. A write request's
// failure means "not sent" only when this says so; every other failure is
// uncertain. Note that a request the adapter could not build is not reported
// as such here — the adapter's own error text carries no type to read — so it
// is treated as uncertain, which is conservative: it can cost a reconcile
// search, never a duplicate.
func beforeSend(err error) bool {
	if err == nil {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}
