package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

// deployResultRecordEventID names the result one deploy record document
// reports. A record is its own report, never matched to a hub attempt: the
// document carries no attempt identity, and service, job, ref and result do
// not establish one (a job can deploy a ref to several targets or retry a
// failure), so sharing an attempt's id could silence a different deploy's
// result. The id covers the key and the fields the notice states, so the same
// document replayed owns one row and a corrected result or job at the same
// key notifies again.
func deployResultRecordEventID(key, target, job, ref, result string) string {
	h := sha256.New()
	for _, f := range []string{"record", key, target, job, ref, result} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return "deploy-result-record-" + hex.EncodeToString(h.Sum(nil)[:16])
}

// laneSafeText maps what the lane-event contract refuses (control characters,
// invalid UTF-8; deploy fields may carry both) to a space, so a notice always
// stays deliverable and never fails the writer's transaction on a NUL.
func laneSafeText(s string) string {
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}

// deployAskerLane is the lane that asked for the job's work, "" when none.
func deployAskerLane(ctx context.Context, tx pgx.Tx, jobID string) (string, error) {
	if jobID == "" || !validText(jobID, 256) || !utf8.ValidString(jobID) {
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
	body := laneSafeText(service + " " + ref + " job " + jobID + ": " + result)
	if failedStep != nil {
		body += " at step " + strconv.Itoa(*failedStep)
	} else if failedStepText != "" {
		body += " at " + laneSafeText(failedStepText)
	}
	_, err = insertNotificationOutboxOnce(ctx, tx, OutboxKindDeployResult, lane, eventID, assistantLaneText("[hk] deploy ", body), at)
	return err
}

// queueDeployRecordNoticeTx is queueDeployResultNoticeTx for a stored deploy
// record document. A document that is not a valid record for its key's
// service, or that names no job, writes nothing. The record owns its own event
// id (see deployResultRecordEventID); a deploy reported both as a hub attempt
// and as a record document therefore notifies once per report, and the notice
// names service, ref and job so the lane can tell them apart.
func queueDeployRecordNoticeTx(ctx context.Context, tx pgx.Tx, x StoredDeployRecord, at time.Time) error {
	m := deployRecordKeyRE.FindStringSubmatch(x.Key)
	if m == nil {
		return nil
	}
	rec, _, reasons := x.Classify(m[1])
	if len(reasons) > 0 || rec.JobID == nil {
		return nil
	}
	ref := "(no ref)"
	if rec.DeployedRef != nil {
		ref = *rec.DeployedRef
	}
	eventID := deployResultRecordEventID(x.Key, rec.Target, *rec.JobID, ref, rec.Result)
	return queueDeployResultNoticeTx(ctx, tx, eventID, rec.Service, *rec.JobID, ref, rec.Result, rec.FailedStep, "", at)
}
