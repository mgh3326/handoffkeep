package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrLabelNotFound = errors.New("label_not_found")
	ErrLabelParent   = errors.New("label_parent_invalid")
	ErrLabelCycle    = errors.New("label_cycle")
	ErrLabelRetired  = errors.New("label_retired")
)

type Label struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Color     string     `json:"color"`
	ParentID  *int64     `json:"parent_id"`
	IsGroup   bool       `json:"is_group"`
	RetiredAt *time.Time `json:"retired_at"`
	EditLevel string     `json:"edit_level"`
}

func (s *Store) SaveLabel(ctx context.Context, l Label) (Label, error) {
	if l.ID < 0 || strings.TrimSpace(l.Name) == "" || !validText(l.Name, 256) || !validText(l.Color, 64) || (l.ParentID != nil && *l.ParentID < 1) || (l.EditLevel != "anyone" && l.EditLevel != "desk" && l.EditLevel != "operator") || l.RetiredAt != nil {
		return Label{}, errors.New("invalid label")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Label{}, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873010)`); err != nil {
		return Label{}, err
	}
	if l.ParentID != nil {
		var group, cycle bool
		err = tx.QueryRow(ctx, `WITH RECURSIVE ancestors AS (SELECT id,parent_id,is_group FROM labels WHERE id=$1 UNION ALL SELECT l.id,l.parent_id,l.is_group FROM labels l JOIN ancestors a ON l.id=a.parent_id) SELECT COALESCE((SELECT is_group FROM ancestors WHERE id=$1),false), EXISTS(SELECT 1 FROM ancestors WHERE id=$2)`, *l.ParentID, l.ID).Scan(&group, &cycle)
		if err != nil {
			return Label{}, err
		}
		if cycle {
			return Label{}, ErrLabelCycle
		}
		if !group {
			return Label{}, ErrLabelParent
		}
	}
	if l.ID == 0 {
		err = tx.QueryRow(ctx, `INSERT INTO labels(name,color,parent_id,is_group,edit_level) VALUES($1,$2,$3,$4,$5) RETURNING id`, l.Name, l.Color, l.ParentID, l.IsGroup, l.EditLevel).Scan(&l.ID)
	} else {
		// A group with children cannot be demoted.
		var hasChildren bool
		if !l.IsGroup {
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM labels WHERE parent_id=$1)`, l.ID).Scan(&hasChildren); err != nil {
				return Label{}, err
			}
			if hasChildren {
				return Label{}, ErrLabelParent
			}
		}
		result, e := tx.Exec(ctx, `UPDATE labels SET name=$2,color=$3,parent_id=$4,is_group=$5,edit_level=$6 WHERE id=$1 AND retired_at IS NULL`, l.ID, l.Name, l.Color, l.ParentID, l.IsGroup, l.EditLevel)
		err = e
		if err == nil && result.RowsAffected() == 0 {
			return Label{}, ErrLabelNotFound
		}
	}
	if err != nil {
		return Label{}, err
	}
	return l, tx.Commit(ctx)
}

func (s *Store) RetireLabel(ctx context.Context, id int64) error {
	if id < 1 {
		return errors.New("invalid label")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE labels SET retired_at=COALESCE(retired_at,$2) WHERE id=$1`, id, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLabelNotFound
	}
	return nil
}

func (s *Store) ListLabels(ctx context.Context) ([]Label, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,color,parent_id,is_group,retired_at,edit_level FROM labels ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Label{}
	for rows.Next() {
		var l Label
		if err = rows.Scan(&l.ID, &l.Name, &l.Color, &l.ParentID, &l.IsGroup, &l.RetiredAt, &l.EditLevel); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) SetTaskLabel(ctx context.Context, id, labelID int64, by string, attach bool) (Task, bool, error) {
	if id < 1 || labelID < 1 || by == "" || !validText(by, 128) {
		return Task{}, false, errors.New("invalid task label")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}
	if err = s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, false, err
	}
	var retired *time.Time
	if err = tx.QueryRow(ctx, `SELECT retired_at FROM labels WHERE id=$1 FOR SHARE`, labelID).Scan(&retired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrLabelNotFound
		}
		return Task{}, false, err
	}
	if attach && retired != nil {
		return Task{}, false, ErrLabelRetired
	}
	var changed bool
	if attach {
		tag, e := tx.Exec(ctx, `INSERT INTO task_labels(task_id,label_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, labelID)
		err = e
		changed = tag.RowsAffected() > 0
	} else {
		tag, e := tx.Exec(ctx, `DELETE FROM task_labels WHERE task_id=$1 AND label_id=$2`, id, labelID)
		err = e
		changed = tag.RowsAffected() > 0
	}
	if err != nil {
		return Task{}, false, err
	}
	if !changed {
		return x, false, tx.Commit(ctx)
	}
	from, to := "", fmt.Sprint(labelID)
	if !attach {
		from, to = to, ""
	}
	now := time.Now().UTC()
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "metadata", From: x.State, To: x.State, By: by, Refs: x.Refs, At: now, Field: "labels", FromValue: from, ToValue: to}); err != nil {
		return Task{}, false, err
	}
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET updated_at=$2 WHERE id=$1 RETURNING `+taskColumns, id, now), &x); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}
