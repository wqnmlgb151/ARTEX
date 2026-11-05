package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MaterializeTimeoutEvents atomically fans out committed events to durable
// deliveries and acknowledges only that fanout. No numeric watermark is used.
func (d *DB) MaterializeTimeoutEvents() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM task_timeout_events WHERE processed_at IS NULL ORDER BY id LIMIT 200 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.Exec(`INSERT INTO timeout_trigger_deliveries(event_id,trigger_id,agent_key,task_id,description,goal,message)
SELECT e.id,tr.id,tr.agent_key,e.task_id,e.description,e.goal,tr.task_timeout_message
FROM task_timeout_events e CROSS JOIN agent_triggers tr
WHERE e.id=$1 AND tr.enabled AND tr.on_task_timeout
ON CONFLICT(event_id,trigger_id) DO NOTHING`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE task_timeout_events SET processed_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type TimeoutDelivery struct {
	ID, TaskID                           int64
	AgentKey, Description, Goal, Message string
}

type TimeoutRun struct {
	ID                       int64
	AgentKey, Title, Message string
}

func (d *DB) PendingTimeoutDeliveries() ([]TimeoutDelivery, error) {
	rows, err := d.Query(`SELECT id,task_id,agent_key,description,goal,message FROM timeout_trigger_deliveries WHERE run_id IS NULL ORDER BY id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeoutDelivery
	for rows.Next() {
		var x TimeoutDelivery
		if err := rows.Scan(&x.ID, &x.TaskID, &x.AgentKey, &x.Description, &x.Goal, &x.Message); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (d *DB) RecoverableTimeoutRuns() ([]TimeoutRun, error) {
	rows, err := d.Query(`SELECT id,agent_key,title,message FROM timeout_trigger_runs WHERE completed_at IS NULL AND lease_until <= now() ORDER BY id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeoutRun
	for rows.Next() {
		var x TimeoutRun
		if err := rows.Scan(&x.ID, &x.AgentKey, &x.Title, &x.Message); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ClaimTimeoutRun persists the queue's merged batch, conversation and human turn
// together. Recovery reuses that conversation. Leases fence concurrent consumers;
// callers must renew while executing and stop on lease loss.
func (d *DB) ClaimTimeoutRun(ids []int64, run TimeoutRun, owner string) (*Conversation, int64, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	if run.ID == 0 {
		if len(ids) == 0 {
			return nil, 0, fmt.Errorf("empty timeout delivery batch")
		}
		// Lock in ID order to avoid deadlocks between overlapping merged batches.
		rows, err := tx.Query(`SELECT id FROM timeout_trigger_deliveries WHERE id=ANY($1) AND run_id IS NULL ORDER BY id FOR UPDATE SKIP LOCKED`, ids)
		if err != nil {
			return nil, 0, err
		}
		n := 0
		for rows.Next() {
			n++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, 0, err
		}
		if n != len(ids) {
			return nil, 0, sql.ErrNoRows
		}
		err = tx.QueryRow(`INSERT INTO timeout_trigger_runs(agent_key,title,message) VALUES($1,$2,$3) RETURNING id`, run.AgentKey, run.Title, run.Message).Scan(&run.ID)
		if err != nil {
			return nil, 0, err
		}
		if _, err = tx.Exec(`UPDATE timeout_trigger_deliveries SET run_id=$1 WHERE id=ANY($2)`, run.ID, ids); err != nil {
			return nil, 0, err
		}
	}
	var convID sql.NullInt64
	err = tx.QueryRow(`UPDATE timeout_trigger_runs SET lease_owner=$2,lease_until=now()+interval '2 minutes',attempts=attempts+1
WHERE id=$1 AND completed_at IS NULL AND lease_until<=now()
RETURNING conversation_id,agent_key,title,message`, run.ID, owner).Scan(&convID, &run.AgentKey, &run.Title, &run.Message)
	if err != nil {
		return nil, 0, err
	}
	var c Conversation
	if convID.Valid {
		c, err = scanConv(tx.QueryRow(`SELECT `+convCols+` FROM conversations WHERE id=$1`, convID.Int64))
	} else {
		c, err = scanConv(tx.QueryRow(`INSERT INTO conversations(agent_key,title) VALUES($1,$2) RETURNING `+convCols, run.AgentKey, run.Title))
		if err == nil {
			_, err = tx.Exec(`INSERT INTO conversation_activities(conversation_id,worker,kind,summary,detail) VALUES($1,$2,'user',$3,$3)`, c.ID, run.AgentKey, run.Message)
		}
		if err == nil {
			_, err = tx.Exec(`UPDATE timeout_trigger_runs SET conversation_id=$2 WHERE id=$1`, run.ID, c.ID)
		}
	}
	if err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, err
	}
	return &c, run.ID, nil
}

func (d *DB) RenewTimeoutRun(id int64, owner string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := d.ExecContext(ctx, `UPDATE timeout_trigger_runs SET lease_until=now()+interval '2 minutes' WHERE id=$1 AND lease_owner=$2 AND completed_at IS NULL AND lease_until>now()`, id, owner)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("timeout run %d lease lost", id)
	}
	return nil
}

func (d *DB) FinishTimeoutRun(id int64, owner string, runErr error) error {
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	res, err := d.Exec(`UPDATE timeout_trigger_runs SET
completed_at=CASE WHEN $3='' THEN now() ELSE NULL END,
last_error=$3,lease_owner='',lease_until=now()+make_interval(secs => LEAST(300,5*attempts))
WHERE id=$1 AND lease_owner=$2 AND completed_at IS NULL AND lease_until>now()`, id, owner, message)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("timeout run %d lease lost at completion", id)
	}
	return nil
}
