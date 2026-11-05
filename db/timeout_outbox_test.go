package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTimeoutDeliveryRecovery(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// A private schema prevents fanout from consuming unrelated test events.
	name := fmt.Sprintf("timeout_test_%d", time.Now().UnixNano())
	if _, err = d.Exec(`CREATE SCHEMA ` + name); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DROP SCHEMA ` + name + ` CASCADE`)
	isolated, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer isolated.Close()
	isolated.SetMaxOpenConns(1)
	if _, err = isolated.Exec(`SET search_path TO ` + name + `,public`); err != nil {
		t.Fatal(err)
	}
	// Only the queue tables need isolation; tasks/agents are shared read-only here.
	if _, err = isolated.Exec(`CREATE TABLE task_timeout_events (LIKE public.task_timeout_events INCLUDING ALL);
CREATE TABLE timeout_trigger_deliveries (LIKE public.timeout_trigger_deliveries INCLUDING ALL);
CREATE TABLE timeout_trigger_runs (LIKE public.timeout_trigger_runs INCLUDING ALL);
CREATE TABLE agent_triggers (LIKE public.agent_triggers INCLUDING ALL);`); err != nil {
		t.Fatal(err)
	}
	q := &DB{isolated}
	if _, err = q.Exec(`INSERT INTO agent_triggers(agent_key,on_task_timeout,task_timeout_message) VALUES('test',true,'test timeout');
INSERT INTO task_timeout_events(task_id,description,goal) VALUES(102,'second','test'),(101,'first','test')`); err != nil {
		t.Fatal(err)
	}
	if err = q.MaterializeTimeoutEvents(); err != nil {
		t.Fatal(err)
	}
	if err = q.MaterializeTimeoutEvents(); err != nil {
		t.Fatal(err)
	}
	items, err := q.PendingTimeoutDeliveries()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].TaskID != 102 || items[1].TaskID != 101 {
		t.Fatalf("out of order deliveries: %+v", items)
	}
	ids := []int64{items[0].ID, items[1].ID}
	run := TimeoutRun{AgentKey: "test", Title: "timeout", Message: "merged timeout"}
	c, id, err := q.ClaimTimeoutRun(ids, run, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = q.ClaimTimeoutRun(ids, run, "other"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate claim: %v", err)
	}
	if err = q.FinishTimeoutRun(id, "first", errors.New("temporary failure")); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Exec(`UPDATE timeout_trigger_runs SET lease_until=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	// A fresh wrapper (scheduler restart) discovers the unfinished run.
	restarted := &DB{isolated}
	runs, err := restarted.RecoverableTimeoutRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("recoverable=%d", len(runs))
	}
	c2, id2, err := restarted.ClaimTimeoutRun(nil, runs[0], "second")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != c2.ID || id != id2 {
		t.Fatal("recovery created a duplicate conversation")
	}
	if err = q.FinishTimeoutRun(id, "first", nil); err == nil {
		t.Fatal("stale owner acknowledged run")
	}
	if err = restarted.FinishTimeoutRun(id, "second", nil); err != nil {
		t.Fatal(err)
	}
	runs, err = q.RecoverableTimeoutRuns()
	if err != nil || len(runs) != 0 {
		t.Fatalf("completed run replayed: %v %+v", err, runs)
	}
	var turns int
	if err = q.QueryRow(`SELECT count(*) FROM conversation_activities WHERE conversation_id=$1 AND kind='user'`, c.ID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Fatalf("human turns=%d", turns)
	}
	defer d.Exec(`DELETE FROM conversations WHERE id=$1`, c.ID)
	// An aborted database transaction must not acknowledge events.
	if _, err = q.Exec(`INSERT INTO task_timeout_events(task_id,description,goal) VALUES(103,'third','test'); DROP TABLE timeout_trigger_deliveries`); err != nil {
		t.Fatal(err)
	}
	if err = q.MaterializeTimeoutEvents(); err == nil {
		t.Fatal("database failure silently acknowledged")
	}
	var pending int
	if err = q.QueryRow(`SELECT count(*) FROM task_timeout_events WHERE processed_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending after failure=%d", pending)
	}
	if _, err = q.Exec(`CREATE TABLE timeout_trigger_deliveries (LIKE public.timeout_trigger_deliveries INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	if err = q.MaterializeTimeoutEvents(); err != nil {
		t.Fatal(err)
	}
	items, err = q.PendingTimeoutDeliveries()
	if err != nil || len(items) != 1 {
		t.Fatalf("recovery after database fault: %v %+v", err, items)
	}
	// Sequence allocation is not commit order: consume the later event while an
	// earlier event is uncommitted, then discover the earlier event after commit.
	slowDB, err := sql.Open("pgx", testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer slowDB.Close()
	slowDB.SetMaxOpenConns(1)
	if _, err = slowDB.Exec(`SET search_path TO ` + name + `,public`); err != nil {
		t.Fatal(err)
	}
	slow, err := slowDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Rollback()
	if _, err = slow.Exec(`INSERT INTO task_timeout_events(task_id,description,goal) VALUES(104,'slow','test')`); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Exec(`INSERT INTO task_timeout_events(task_id,description,goal) VALUES(105,'fast','test')`); err != nil {
		t.Fatal(err)
	}
	if err = q.MaterializeTimeoutEvents(); err != nil {
		t.Fatal(err)
	}
	if err = slow.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = q.MaterializeTimeoutEvents(); err != nil {
		t.Fatal(err)
	}
	var reordered int
	if err = q.QueryRow(`SELECT count(*) FROM timeout_trigger_deliveries WHERE task_id IN (104,105)`).Scan(&reordered); err != nil {
		t.Fatal(err)
	}
	if reordered != 2 {
		t.Fatalf("commit order lost events: %d", reordered)
	}
}

func TestTimeoutOutboxTransitions(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	create := func() *Task {
		x, err := d.CreateTask("timeout outbox", "test", nil, 0, 300)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.DeleteTask(x.ID) })
		return x
	}
	a, b := create(), create()
	count := func(id int64, want int) {
		t.Helper()
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM task_timeout_events WHERE task_id=$1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("task %d events=%d want=%d", id, n, want)
		}
	}
	// The higher task ID times out first, then the lower ID.
	for _, x := range []*Task{b, a} {
		if _, err := d.SetTerminalStatusGuarded(x.ID, "timeout"); err != nil {
			t.Fatal(err)
		}
	}
	count(a.ID, 1)
	count(b.ID, 1)
	if _, err := d.SetTerminalStatusGuarded(a.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	count(a.ID, 1)
	if err := d.SetStatus(a.ID, "running"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SetTerminalStatusGuarded(a.ID, "timeout"); err != nil {
		t.Fatal(err)
	}
	count(a.ID, 2)
	for _, status := range []string{"done", "failed", "paused"} {
		x := create()
		if err := d.SetStatus(x.ID, status); err != nil {
			t.Fatal(err)
		}
		count(x.ID, 0)
	}
	// Terminal writers race: only a winning timeout may produce an event.
	x := create()
	var wg sync.WaitGroup
	for _, status := range []string{"done", "timeout"} {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			if _, err := d.SetTerminalStatusGuarded(x.ID, status); err != nil {
				t.Error(err)
			}
		}(status)
	}
	wg.Wait()
	var status string
	if err := d.QueryRow(`SELECT status FROM tasks WHERE id=$1`, x.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	want := 0
	if status == "timeout" {
		want = 1
	}
	count(x.ID, want)
	// Reapplying schema must not backfill existing timeout tasks.
	if err := withSchemaMigrationLock(t.Context(), d.DB, func(conn *sql.Conn) error { _, err := conn.ExecContext(t.Context(), schemaSQL); return err }); err != nil {
		t.Fatal(fmt.Errorf("repeat schema: %w", err))
	}
	count(a.ID, 2)
	count(b.ID, 1)
	// Concurrent task status writers generate one event each, independent of IDs.
	var concurrent []*Task
	for i := 0; i < 24; i++ {
		concurrent = append(concurrent, create())
	}
	for _, task := range concurrent {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if _, err := d.SetTerminalStatusGuarded(id, "timeout"); err != nil {
				t.Error(err)
			}
		}(task.ID)
	}
	wg.Wait()
	for _, task := range concurrent {
		count(task.ID, 1)
	}
	rolled := create()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE tasks SET status='timeout' WHERE id=$1`, rolled.ID); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	count(rolled.ID, 0)
}
