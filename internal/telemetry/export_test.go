package telemetry

import (
	"github.com/getsentry/sentry-go"
)

// ObserveSamplingForTest reports every transaction head sampling decision to
// observe until the returned function restores the previous observer.
func ObserveSamplingForTest(observe func(name string, rate float64)) (restore func()) {
	previous := sampleObserver
	sampleObserver = observe
	return func() { sampleObserver = previous }
}

// FromEnvWithTransportForTest is FromEnv with the Sentry transport replaced, so
// a test can record envelopes without any network.
func FromEnvWithTransportForTest(release string, transport sentry.Transport) (*Monitor, error) {
	return fromEnv(release, transport)
}
