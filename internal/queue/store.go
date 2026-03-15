package queue

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type Store struct {
	db            *sql.DB
	leaseDuration int
	retryWait     int
}

func NewStore(dsn string, leaseDuration, retryWait int) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	return &Store{
		db:            db,
		leaseDuration: leaseDuration,
		retryWait:     retryWait,
	}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Enqueue(ctx context.Context, req *EnqueueRequest) (*Task, error) {
	priority := PriorityDefault
	if req.Priority != nil {
		priority = *req.Priority
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Unix()

	res, err := tx.ExecContext(ctx,
		"INSERT INTO worker_taskdata (data) VALUES (?)",
		req.Data)
	if err != nil {
		return nil, fmt.Errorf("insert taskdata: %w", err)
	}
	dataID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get taskdata id: %w", err)
	}

	var leaseExpires *int64
	if req.DelayUntil != nil && *req.DelayUntil > 0 {
		leaseExpires = req.DelayUntil
	}

	res, err = tx.ExecContext(ctx,
		`INSERT INTO worker_activetask
			(taskClass, leaseOwner, leaseExpires, failureCount, dataID, failureTime,
			 priority, objectPHID, containerPHID, dateCreated, dateModified)
		VALUES (?, NULL, ?, 0, ?, NULL, ?, ?, ?, ?, ?)`,
		req.TaskClass,
		leaseExpires,
		dataID,
		priority,
		nullStr(req.ObjectPHID),
		nullStr(req.ContainerPHID),
		now,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert activetask: %w", err)
	}

	taskID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get activetask id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	task := &Task{
		ID:            taskID,
		TaskClass:     req.TaskClass,
		DataID:        dataID,
		Priority:      priority,
		ObjectPHID:    req.ObjectPHID,
		ContainerPHID: req.ContainerPHID,
		DateCreated:   now,
		DateModified:  now,
		LeaseExpires:  leaseExpires,
	}
	return task, nil
}

func (s *Store) Lease(ctx context.Context, limit int, leaseOwner string) ([]*Task, error) {
	if limit <= 0 {
		limit = 1
	}

	now := time.Now().Unix()
	leaseExp := now + int64(s.leaseDuration)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var tasks []*Task

	// Phase 1: unleased tasks (new tasks first)
	if len(tasks) < limit {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM worker_activetask
			 WHERE leaseOwner IS NULL
			 ORDER BY priority ASC, id ASC
			 LIMIT ?`,
			limit-len(tasks))
		if err != nil {
			return nil, fmt.Errorf("select unleased: %w", err)
		}
		ids := scanIDs(rows)
		_ = rows.Close()

		if len(ids) > 0 {
			res, err := tx.ExecContext(ctx,
				fmt.Sprintf(
					`UPDATE worker_activetask
					 SET leaseOwner = ?, leaseExpires = ?
					 WHERE leaseOwner IS NULL AND id IN (%s)`,
					placeholders(len(ids))),
				appendArgs(leaseOwner, leaseExp, ids)...)
			if err != nil {
				return nil, fmt.Errorf("update unleased: %w", err)
			}
			n, _ := res.RowsAffected()
			_ = n
		}
	}

	// Phase 2: expired leases (retry failed tasks)
	remaining := limit - len(tasks)
	if remaining > 0 {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM worker_activetask
			 WHERE leaseExpires < ?
			 ORDER BY leaseExpires ASC
			 LIMIT ?`,
			now, remaining)
		if err != nil {
			return nil, fmt.Errorf("select expired: %w", err)
		}
		ids := scanIDs(rows)
		_ = rows.Close()

		if len(ids) > 0 {
			_, err := tx.ExecContext(ctx,
				fmt.Sprintf(
					`UPDATE worker_activetask
					 SET leaseOwner = ?, leaseExpires = ?
					 WHERE leaseExpires < ? AND id IN (%s)`,
					placeholders(len(ids))),
				appendArgs(leaseOwner, leaseExp, now, ids)...)
			if err != nil {
				return nil, fmt.Errorf("update expired: %w", err)
			}
		}
	}

	// Fetch all tasks we just leased, joined with their data
	fetchRows, err := tx.QueryContext(ctx,
		`SELECT t.id, t.taskClass, t.leaseOwner, t.leaseExpires,
		        t.failureCount, t.dataID, t.failureTime,
		        t.priority, t.objectPHID, t.containerPHID,
		        t.dateCreated, t.dateModified,
		        COALESCE(d.data, '')
		 FROM worker_activetask t
		 LEFT JOIN worker_taskdata d ON d.id = t.dataID
		 WHERE t.leaseOwner = ? AND t.leaseExpires > ?
		 ORDER BY t.priority ASC, t.id ASC
		 LIMIT ?`,
		leaseOwner, now, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch leased: %w", err)
	}
	defer func() { _ = fetchRows.Close() }()

	for fetchRows.Next() {
		t := &Task{}
		var leaseOwnerN, objectPHID, containerPHID sql.NullString
		var leaseExp, failureTime sql.NullInt64
		err := fetchRows.Scan(
			&t.ID, &t.TaskClass, &leaseOwnerN, &leaseExp,
			&t.FailureCount, &t.DataID, &failureTime,
			&t.Priority, &objectPHID, &containerPHID,
			&t.DateCreated, &t.DateModified,
			&t.Data,
		)
		if err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		if leaseOwnerN.Valid {
			t.LeaseOwner = leaseOwnerN.String
		}
		if leaseExp.Valid {
			t.LeaseExpires = &leaseExp.Int64
		}
		if failureTime.Valid {
			t.FailureTime = &failureTime.Int64
		}
		if objectPHID.Valid {
			t.ObjectPHID = objectPHID.String
		}
		if containerPHID.Valid {
			t.ContainerPHID = containerPHID.String
		}
		tasks = append(tasks, t)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return tasks, nil
}

func (s *Store) Complete(ctx context.Context, taskID int64, duration int64) (*ArchivedTask, error) {
	return s.archiveTask(ctx, taskID, ResultSuccess, duration)
}

func (s *Store) Fail(ctx context.Context, req *FailRequest) error {
	if req.Permanent {
		_, err := s.archiveTask(ctx, req.TaskID, ResultFailure, 0)
		return err
	}

	retryWait := s.retryWait
	if req.RetryWait != nil {
		retryWait = *req.RetryWait
	}

	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE worker_activetask
		 SET failureCount = failureCount + 1,
		     failureTime = ?,
		     leaseOwner = NULL,
		     leaseExpires = ?,
		     dateModified = ?
		 WHERE id = ?`,
		now,
		now+int64(retryWait),
		now,
		req.TaskID,
	)
	return err
}

func (s *Store) Yield(ctx context.Context, taskID int64, duration int) error {
	if duration < 5 {
		duration = 5
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE worker_activetask
		 SET leaseOwner = ?,
		     leaseExpires = ?,
		     dateModified = ?
		 WHERE id = ?`,
		YieldOwner,
		now+int64(duration),
		now,
		taskID,
	)
	return err
}

func (s *Store) Cancel(ctx context.Context, taskID int64) (*ArchivedTask, error) {
	return s.archiveTask(ctx, taskID, ResultCancelled, 0)
}

func (s *Store) Awaken(ctx context.Context, taskIDs []int64) (int64, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}

	window := int64(3600) // 1 hour
	epochAgo := time.Now().Unix() - window

	res, err := s.db.ExecContext(ctx,
		fmt.Sprintf(
			`UPDATE worker_activetask
			 SET leaseExpires = ?
			 WHERE id IN (%s)
			   AND leaseOwner = ?
			   AND leaseExpires > ?
			   AND failureCount = 0`,
			placeholders(len(taskIDs))),
		appendArgs(epochAgo, taskIDs, YieldOwner, epochAgo)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) Stats(ctx context.Context) (*QueueStats, error) {
	stats := &QueueStats{}
	now := time.Now().Unix()

	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_activetask").Scan(&stats.ActiveCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_activetask WHERE leaseOwner IS NOT NULL AND leaseExpires >= ?",
		now).Scan(&stats.LeasedCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_archivetask").Scan(&stats.ArchivedCount)
	if err != nil {
		return nil, err
	}

	err = s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM worker_activetask WHERE failureCount > 0").Scan(&stats.FailedCount)
	if err != nil {
		return nil, err
	}

	return stats, nil
}

func (s *Store) GetTask(ctx context.Context, taskID int64) (*Task, error) {
	t := &Task{}
	var leaseOwnerN, objectPHID, containerPHID sql.NullString
	var leaseExp, failureTime sql.NullInt64

	err := s.db.QueryRowContext(ctx,
		`SELECT t.id, t.taskClass, t.leaseOwner, t.leaseExpires,
		        t.failureCount, t.dataID, t.failureTime,
		        t.priority, t.objectPHID, t.containerPHID,
		        t.dateCreated, t.dateModified,
		        COALESCE(d.data, '')
		 FROM worker_activetask t
		 LEFT JOIN worker_taskdata d ON d.id = t.dataID
		 WHERE t.id = ?`,
		taskID).Scan(
		&t.ID, &t.TaskClass, &leaseOwnerN, &leaseExp,
		&t.FailureCount, &t.DataID, &failureTime,
		&t.Priority, &objectPHID, &containerPHID,
		&t.DateCreated, &t.DateModified,
		&t.Data,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if leaseOwnerN.Valid {
		t.LeaseOwner = leaseOwnerN.String
	}
	if leaseExp.Valid {
		t.LeaseExpires = &leaseExp.Int64
	}
	if failureTime.Valid {
		t.FailureTime = &failureTime.Int64
	}
	if objectPHID.Valid {
		t.ObjectPHID = objectPHID.String
	}
	if containerPHID.Valid {
		t.ContainerPHID = containerPHID.String
	}
	return t, nil
}

func (s *Store) ListActive(ctx context.Context, limit, offset int) ([]*Task, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.taskClass, t.leaseOwner, t.leaseExpires,
		        t.failureCount, t.dataID, t.failureTime,
		        t.priority, t.objectPHID, t.containerPHID,
		        t.dateCreated, t.dateModified
		 FROM worker_activetask t
		 ORDER BY t.priority ASC, t.id ASC
		 LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tasks []*Task
	for rows.Next() {
		t := &Task{}
		var leaseOwnerN, objectPHID, containerPHID sql.NullString
		var leaseExp, failureTime sql.NullInt64
		err := rows.Scan(
			&t.ID, &t.TaskClass, &leaseOwnerN, &leaseExp,
			&t.FailureCount, &t.DataID, &failureTime,
			&t.Priority, &objectPHID, &containerPHID,
			&t.DateCreated, &t.DateModified,
		)
		if err != nil {
			return nil, err
		}
		if leaseOwnerN.Valid {
			t.LeaseOwner = leaseOwnerN.String
		}
		if leaseExp.Valid {
			t.LeaseExpires = &leaseExp.Int64
		}
		if failureTime.Valid {
			t.FailureTime = &failureTime.Int64
		}
		if objectPHID.Valid {
			t.ObjectPHID = objectPHID.String
		}
		if containerPHID.Valid {
			t.ContainerPHID = containerPHID.String
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (s *Store) archiveTask(ctx context.Context, taskID int64, result int, duration int64) (*ArchivedTask, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var t Task
	var leaseOwnerN, objectPHID, containerPHID sql.NullString
	var leaseExp, failureTime sql.NullInt64

	err = tx.QueryRowContext(ctx,
		`SELECT id, taskClass, leaseOwner, leaseExpires,
		        failureCount, dataID, failureTime,
		        priority, objectPHID, containerPHID,
		        dateCreated, dateModified
		 FROM worker_activetask WHERE id = ?`,
		taskID).Scan(
		&t.ID, &t.TaskClass, &leaseOwnerN, &leaseExp,
		&t.FailureCount, &t.DataID, &failureTime,
		&t.Priority, &objectPHID, &containerPHID,
		&t.DateCreated, &t.DateModified,
	)
	if err != nil {
		return nil, fmt.Errorf("select activetask: %w", err)
	}

	if leaseOwnerN.Valid {
		t.LeaseOwner = leaseOwnerN.String
	}
	if objectPHID.Valid {
		t.ObjectPHID = objectPHID.String
	}
	if containerPHID.Valid {
		t.ContainerPHID = containerPHID.String
	}

	now := time.Now().Unix()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO worker_archivetask
			(id, taskClass, leaseOwner, leaseExpires, failureCount, dataID,
			 failureTime, priority, objectPHID, containerPHID,
			 result, duration, dateCreated, dateModified, archivedEpoch)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.TaskClass,
		nullStr(t.LeaseOwner), leaseExp,
		t.FailureCount, t.DataID,
		failureTime, t.Priority,
		nullStr(t.ObjectPHID), nullStr(t.ContainerPHID),
		result, duration,
		t.DateCreated, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert archivetask: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		"DELETE FROM worker_activetask WHERE id = ?", taskID)
	if err != nil {
		return nil, fmt.Errorf("delete activetask: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	archived := &ArchivedTask{
		Task:          t,
		Result:        result,
		Duration:      duration,
		ArchivedEpoch: &now,
	}
	return archived, nil
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func scanIDs(rows *sql.Rows) []int64 {
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func appendArgs(args ...interface{}) []interface{} {
	var out []interface{}
	for _, a := range args {
		switch v := a.(type) {
		case []int64:
			for _, id := range v {
				out = append(out, id)
			}
		default:
			out = append(out, a)
		}
	}
	return out
}
