// Package capabilities names the /v1 request shapes this build accepts. The
// server answers them on GET /v1/capabilities, and the CLI reads that answer
// before it sends a shape an older server would refuse. The package imports
// nothing, so the CLI can use it without pulling in the server.
package capabilities

// ProtocolVersion is the /v1 protocol revision this build speaks. It rises only
// when a change cannot be expressed as one more name in Features.
const ProtocolVersion = 1

// Each feature is a request shape: a body field an older server would refuse
// with invalid_context. The value is the name the CLI reports in
// server_lacks_feature:<name>.
const (
	FeatureClaimJobID         = "claim_job_id"
	FeatureClaimNoJob         = "claim_no_job"
	FeatureNextJobID          = "next_job_id"
	FeatureNextNoJob          = "next_no_job"
	FeatureTransitionNoJob    = "transition_no_job"
	FeatureResolveNoJob       = "resolve_no_job"
	FeatureDispositionProject = "disposition_project"
)

// Features lists every feature this build accepts. Every Feature constant above
// must appear here; the api package's test checks it against the source.
var Features = []string{
	FeatureClaimJobID,
	FeatureClaimNoJob,
	FeatureNextJobID,
	FeatureNextNoJob,
	FeatureTransitionNoJob,
	FeatureResolveNoJob,
	FeatureDispositionProject,
}
