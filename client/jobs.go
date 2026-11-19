package client

import (
	"context"
	"net/url"
)

// Job is the fleet view of a job record: the fields a fleet service reads,
// with the owner fence (hk 5276). OwnerNode "" and OwnerEpoch 0 mean no owner.
type Job struct {
	JobID      string `json:"job_id"`
	TaskID     *int64 `json:"task_id"`
	Host       string `json:"host"`
	State      string `json:"state"`
	OwnerNode  string `json:"owner_node"`
	OwnerEpoch int64  `json:"owner_epoch"`
}

// JobOwner is one owner fence: a node and the epoch of its grant.
type JobOwner struct {
	Node  string `json:"node"`
	Epoch int64  `json:"epoch"`
}

// JobUpdate is the fleet subset of PUT /v1/jobs/{job}: the state and the owner
// fence. A nil field is sent as null, which the server reads as absent.
// Owner sets the fence and its epoch must move forward. ExpectedOwner makes
// the whole update conditional: when it differs from the stored owner the
// server refuses the update and applies none of it. Both together transfer
// the job atomically.
type JobUpdate struct {
	State         *string   `json:"state"`
	Owner         *JobOwner `json:"owner"`
	ExpectedOwner *JobOwner `json:"expected_owner"`
}

// GetJob reads one job. found is false when the job does not exist.
func (c Client) GetJob(ctx context.Context, id string) (Job, bool, error) {
	var out Job
	err := c.transport().Call(ctx, "GET", "/v1/jobs/"+url.PathEscape(id), nil, &out)
	if ErrorCode(err) == "not_found" {
		return out, false, nil
	}
	return out, err == nil, err
}

// UpdateJob sends one conditional or unconditional update. A refused owner
// fence is an error whose ErrorCode is "job_owner_mismatch" (409); read the
// current owner with GetJob. Branch on the code, never on the error text.
func (c Client) UpdateJob(ctx context.Context, id string, in JobUpdate) (Job, error) {
	var out Job
	err := c.transport().Call(ctx, "PUT", "/v1/jobs/"+url.PathEscape(id), in, &out)
	return out, err
}
