package api

import "errors"

// The deployment may override the observed workspace entitlement without
// changing code. The creation margin is always five issue slots.
type LinearCapacityPolicy struct{ limit, high, low int }

func NewLinearCapacityPolicy(limit, high, low int) (LinearCapacityPolicy, error) {
	if limit <= 5 || low < 0 || high <= low || high >= limit-5 {
		return LinearCapacityPolicy{}, errors.New("invalid Linear capacity policy: require 0 <= low < high < limit-5")
	}
	return LinearCapacityPolicy{limit: limit, high: high, low: low}, nil
}
func (p LinearCapacityPolicy) Limit() int {
	if p.limit == 0 {
		return 275
	}
	return p.limit
}
func (p LinearCapacityPolicy) CreateCeiling() int { return p.Limit() - 5 }
func (p LinearCapacityPolicy) ArchiveHigh() int {
	if p.limit == 0 {
		return 230
	}
	return p.high
}
func (p LinearCapacityPolicy) ArchiveLow() int {
	if p.limit == 0 {
		return 200
	}
	return p.low
}
