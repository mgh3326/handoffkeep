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

// deployResultEventID names one result of one job's deploy of one ref. The
// structured finish (FinishDeploy) and the deploy-record document both derive
// it from the same fields, so a deploy reported through both paths owns one
// row.
func deployResultEventID(service, jobID, ref, result string) string {
	id := "deploy-result-" + service + "-" + jobID + "-" + ref + "-" + result
	if len(id) <= 200 {
		return id
	}
	// A job id and a ref are each up to 500 bytes; an event id is at most 256
	// (MarkNotificationSent), so a long one keys on the hash of its parts.
	sum := sha256.Sum256([]byte(service + "\x00" + jobID + "\x00" + ref + "\x00" + result))
	return "deploy-result-" + hex.EncodeToString(sum[:16])
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
// the writer's transaction. The text is a record pointer: service, ref, job
// and the state, with the failed step when there is one.
func queueDeployResultNoticeTx(ctx context.Context, tx pgx.Tx, service, jobID, ref, result string, failedStep *int, failedStepText string, at time.Time) error {
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
	_, err = insertNotificationOutboxOnce(ctx, tx, OutboxKindDeployResult, lane, deployResultEventID(service, jobID, ref, result), assistantLaneText("[hk] deploy ", body), at)
	return err
}

// queueDeployRecordNoticeTx is queueDeployResultNoticeTx for a stored deploy
// record document. A document that is not a valid record for its key's
// service, or that names no job, writes nothing.
func queueDeployRecordNoticeTx(ctx context.Context, tx pgx.Tx, x StoredDeployRecord, at time.Time) error {
	m := deployRecordKeyRE.FindStringSubmatch(x.Key)
	if m == nil {
		return nil
	}
	rec, _, reasons := x.Classify(m[1])
	if len(reasons) > 0 || rec.JobID == nil || rec.DeployedRef == nil {
		return nil
	}
	return queueDeployResultNoticeTx(ctx, tx, rec.Service, *rec.JobID, *rec.DeployedRef, rec.Result, rec.FailedStep, "", at)
}
