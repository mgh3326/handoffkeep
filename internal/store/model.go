package store

import "time"

func slaRisk(now time.Time, started, medium, high, breach *time.Time) string {
	if started == nil || now.Before(*started) {
		return "none"
	}
	if breach != nil && !now.Before(*breach) {
		return "breached"
	}
	if high != nil && !now.Before(*high) {
		return "high"
	}
	if medium != nil && !now.Before(*medium) {
		return "medium"
	}
	return "none"
}
