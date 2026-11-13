package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// A deploy result is a resolution too (hk 3295): the lane that asked for the
// deploy hears one notice when its attempt ends or its deploy record lands.
// The asker is the lane that spawned the deploy's job (parent_lane, else the
// job's own lane); a deploy with no job, or a job no lane is recorded for,
// has no asker and writes nothing.

// deployResultAttemptEventID names the result of one deploy attempt. The
// attempt id is one field, so the id cannot collide the way a join of several
// free-text fields (service, job, ref) could; it is hashed to stay within the
// 256-byte event id cap whatever the attempt id's length.
func deployResultAttemptEventID(attemptID string) string {
	sum := sha256.Sum256([]byte("attempt\x00" + attemptID))
	return "deploy-result-attempt-" + hex.EncodeToString(sum[:16])
}

// deployResultRecordEventID names the result a deploy record document reports
// when no attempt of the hub matches it: the record key is its identity.
func deployResultRecordEventID(key string) string {
	sum := sha256.Sum256([]byte("record\x00" + key))
	return "deploy-result-record-" + hex.EncodeToString(sum[:16])
}

// deployAskerLane is the lane that asked for the job's work, "" when none.
func deployAskerLane(ctx context.Context, tx pgx.Tx, jobID string) (string, error) {
	if jobID == "" {
		return "", nil
	}
	var parent, owner string
	err := tx.QueryRow(ctx, `SELECT parent_lane,owner_lane FROM jobs WHERE job_id=$1`, jobID).Scan(&parent, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if parent != "" {
		return parent, nil
	}
	return owner, nil
}

// queueDeployResultNoticeTx writes the notice for one deploy result inside
// the writer's transaction under eventID. The text is a record pointer: service, ref, job
// and the state, with the failed step when there is one.
func queueDeployResultNoticeTx(ctx context.Context, tx pgx.Tx, eventID, service, jobID, ref, result string, failedStep *int, failedStepText string, at time.Time) error {
	lane, err := deployAskerLane(ctx, tx, jobID)
	if err != nil || lane == "" {
		return err
	}
	body := service + " " + ref + " job " + jobID + ": " + result
	if failedStep != nil {
		body += " at step " + strconv.Itoa(*failedStep)
	} else if failedStepText != "" {
		body += " at " + failedStepText
	}
	_, err = insertNotificationOutboxOnce(ctx, tx, OutboxKindDeployResult, lane, eventID, assistantLaneText("[hk] deploy ", body), at)
	return err
}

// queueDeployRecordNoticeTx is queueDeployResultNoticeTx for a stored deploy
// record document. A document that is not a valid record for its key's
// service, or that names no job, writes nothing. A record that matches a hub
// attempt (same service, job, ref and result) shares that attempt's event id,
// so a deploy reported through both paths owns one row; any other record is
// identified by its key, which also covers a record without a ref.
func queueDeployRecordNoticeTx(ctx context.Context, tx pgx.Tx, x StoredDeployRecord, at time.Time) error {
	m := deployRecordKeyRE.FindStringSubmatch(x.Key)
	if m == nil {
		return nil
	}
	rec, _, reasons := x.Classify(m[1])
	if len(reasons) > 0 || rec.JobID == nil {
		return nil
	}
	eventID, ref := deployResultRecordEventID(x.Key), "(no ref)"
	if rec.DeployedRef != nil {
		ref = *rec.DeployedRef
		var attemptID string
		err := tx.QueryRow(ctx, `SELECT attempt_id FROM deploys WHERE service=$1 AND job_id=$2 AND deployed_ref=$3 AND state=$4 ORDER BY id DESC LIMIT 1`, rec.Service, *rec.JobID, ref, rec.Result).Scan(&attemptID)
		switch {
		case err == nil:
			eventID = deployResultAttemptEventID(attemptID)
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
	}
	return queueDeployResultNoticeTx(ctx, tx, eventID, rec.Service, *rec.JobID, ref, rec.Result, rec.FailedStep, "", at)
}
