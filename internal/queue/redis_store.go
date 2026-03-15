package queue

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	rdb           *redis.Client
	prefix        string
	leaseDuration int
	retryWait     int
}

func NewRedisStore(addr, password string, db int, prefix string, leaseDuration, retryWait int) (*RedisStore, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:         addr,
		Password:     password,
		DB:           db,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		PoolSize:     25,
		MinIdleConns: 5,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &RedisStore{
		rdb:           rdb,
		prefix:        prefix,
		leaseDuration: leaseDuration,
		retryWait:     retryWait,
	}, nil
}

func (s *RedisStore) Close() error {
	return s.rdb.Close()
}

func (s *RedisStore) key(parts ...string) string {
	k := s.prefix
	for _, p := range parts {
		k += p
	}
	return k
}

func (s *RedisStore) taskKey(id int64) string { return s.key("task:", strconv.FormatInt(id, 10)) }
func (s *RedisStore) dataKey(id int64) string { return s.key("data:", strconv.FormatInt(id, 10)) }
func (s *RedisStore) archivedKey(id int64) string {
	return s.key("archived:", strconv.FormatInt(id, 10))
}
func (s *RedisStore) activeSetKey() string       { return s.key("idx:active") }
func (s *RedisStore) unleasedSetKey() string     { return s.key("idx:unleased") }
func (s *RedisStore) leasedSetKey() string       { return s.key("idx:leased") }
func (s *RedisStore) archivedIndexKey() string   { return s.key("idx:archived") }
func (s *RedisStore) nextTaskIDKey() string      { return s.key("next_task_id") }
func (s *RedisStore) nextDataIDKey() string      { return s.key("next_data_id") }
func (s *RedisStore) failedCounterKey() string   { return s.key("counter:failed") }
func (s *RedisStore) archivedCounterKey() string { return s.key("counter:archived") }

// activeScore produces a deterministic sort key: lower priority number = higher priority,
// tie-broken by task ID ascending.
func activeScore(priority int, id int64) float64 {
	return float64(priority)*1e12 + float64(id)
}

func (s *RedisStore) Enqueue(ctx context.Context, req *EnqueueRequest) (*Task, error) {
	priority := PriorityDefault
	if req.Priority != nil {
		priority = *req.Priority
	}

	now := time.Now().Unix()

	dataID, err := s.rdb.Incr(ctx, s.nextDataIDKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("incr data id: %w", err)
	}

	taskID, err := s.rdb.Incr(ctx, s.nextTaskIDKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("incr task id: %w", err)
	}

	var leaseExpires *int64
	if req.DelayUntil != nil && *req.DelayUntil > 0 {
		leaseExpires = req.DelayUntil
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

	fields := taskToMap(task)

	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, s.dataKey(dataID), req.Data, 0)
	pipe.HSet(ctx, s.taskKey(taskID), fields)
	pipe.ZAdd(ctx, s.activeSetKey(), redis.Z{Score: activeScore(priority, taskID), Member: taskID})
	if leaseExpires == nil {
		pipe.ZAdd(ctx, s.unleasedSetKey(), redis.Z{Score: activeScore(priority, taskID), Member: taskID})
	} else {
		pipe.ZAdd(ctx, s.leasedSetKey(), redis.Z{Score: float64(*leaseExpires), Member: taskID})
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("enqueue pipeline: %w", err)
	}

	return task, nil
}

var leaseScript = redis.NewScript(`
local unleasedKey   = KEYS[1]
local leasedKey     = KEYS[2]
local limit         = tonumber(ARGV[1])
local leaseOwner    = ARGV[2]
local leaseExpires  = tonumber(ARGV[3])
local now           = tonumber(ARGV[4])
local taskKeyPrefix = ARGV[5]

local ids = {}

-- Phase 1: pick unleased tasks (sorted by priority+id)
local unleased = redis.call('ZRANGE', unleasedKey, 0, limit - 1)
for _, mid in ipairs(unleased) do
    local tk = taskKeyPrefix .. mid
    redis.call('HSET', tk, 'leaseOwner', leaseOwner, 'leaseExpires', leaseExpires, 'dateModified', now)
    redis.call('ZREM', unleasedKey, mid)
    redis.call('ZADD', leasedKey, leaseExpires, mid)
    table.insert(ids, mid)
end

-- Phase 2: pick tasks with expired leases
local remaining = limit - #ids
if remaining > 0 then
    local expired = redis.call('ZRANGEBYSCORE', leasedKey, '-inf', now, 'LIMIT', 0, remaining)
    for _, mid in ipairs(expired) do
        local tk = taskKeyPrefix .. mid
        redis.call('HSET', tk, 'leaseOwner', leaseOwner, 'leaseExpires', leaseExpires, 'dateModified', now)
        redis.call('ZADD', leasedKey, leaseExpires, mid)
        table.insert(ids, mid)
    end
end

return ids
`)

func (s *RedisStore) Lease(ctx context.Context, limit int, leaseOwner string) ([]*Task, error) {
	if limit <= 0 {
		limit = 1
	}

	now := time.Now().Unix()
	leaseExp := now + int64(s.leaseDuration)

	result, err := leaseScript.Run(ctx, s.rdb,
		[]string{s.unleasedSetKey(), s.leasedSetKey()},
		limit, leaseOwner, leaseExp, now, s.key("task:"),
	).StringSlice()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("lease script: %w", err)
	}

	if len(result) == 0 {
		return []*Task{}, nil
	}

	tasks := make([]*Task, 0, len(result))
	for _, idStr := range result {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		t, err := s.loadTask(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load task %d: %w", id, err)
		}
		if t != nil {
			tasks = append(tasks, t)
		}
	}

	return tasks, nil
}

var archiveScript = redis.NewScript(`
local taskKey       = KEYS[1]
local archivedKey   = KEYS[2]
local activeSet     = KEYS[3]
local unleasedSet   = KEYS[4]
local leasedSet     = KEYS[5]
local archivedIndex = KEYS[6]
local archivedCounter = KEYS[7]
local dataKey       = KEYS[8]

local taskID   = ARGV[1]
local result   = ARGV[2]
local duration = ARGV[3]
local now      = ARGV[4]

if redis.call('EXISTS', taskKey) == 0 then
    return redis.error_reply('task not found')
end

-- Copy all fields to the archived hash
local fields = redis.call('HGETALL', taskKey)
for i = 1, #fields, 2 do
    redis.call('HSET', archivedKey, fields[i], fields[i+1])
end
redis.call('HSET', archivedKey, 'result', result, 'duration', duration, 'archivedEpoch', now, 'dateModified', now)

-- Copy data payload
local data = redis.call('GET', dataKey)
if data then
    redis.call('HSET', archivedKey, 'data', data)
end

-- Clean up active structures
redis.call('ZREM', activeSet, taskID)
redis.call('ZREM', unleasedSet, taskID)
redis.call('ZREM', leasedSet, taskID)
redis.call('DEL', taskKey)

-- Add to archived index and increment counter
redis.call('ZADD', archivedIndex, now, taskID)
redis.call('INCR', archivedCounter)

-- Check if failed and decrement failed counter
local fc = redis.call('HGET', archivedKey, 'failureCount')
if fc and tonumber(fc) > 0 then
    redis.call('DECR', KEYS[9])
end

return 1
`)

func (s *RedisStore) Complete(ctx context.Context, taskID int64, duration int64) (*ArchivedTask, error) {
	return s.archiveTask(ctx, taskID, ResultSuccess, duration)
}

func (s *RedisStore) Cancel(ctx context.Context, taskID int64) (*ArchivedTask, error) {
	return s.archiveTask(ctx, taskID, ResultCancelled, 0)
}

func (s *RedisStore) archiveTask(ctx context.Context, taskID int64, result int, duration int64) (*ArchivedTask, error) {
	t, err := s.loadTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("select activetask: task %d not found", taskID)
	}

	now := time.Now().Unix()

	err = archiveScript.Run(ctx, s.rdb,
		[]string{
			s.taskKey(taskID),
			s.archivedKey(taskID),
			s.activeSetKey(),
			s.unleasedSetKey(),
			s.leasedSetKey(),
			s.archivedIndexKey(),
			s.archivedCounterKey(),
			s.dataKey(t.DataID),
			s.failedCounterKey(),
		},
		taskID, result, duration, now,
	).Err()
	if err != nil {
		return nil, fmt.Errorf("archive task: %w", err)
	}

	return &ArchivedTask{
		Task:          *t,
		Result:        result,
		Duration:      duration,
		ArchivedEpoch: &now,
	}, nil
}

var failScript = redis.NewScript(`
local taskKey     = KEYS[1]
local unleasedSet = KEYS[2]
local leasedSet   = KEYS[3]
local failedCtr   = KEYS[4]

local taskID       = ARGV[1]
local now          = ARGV[2]
local leaseExpires = ARGV[3]

if redis.call('EXISTS', taskKey) == 0 then
    return redis.error_reply('task not found')
end

local prevFC = tonumber(redis.call('HGET', taskKey, 'failureCount') or '0')
redis.call('HSET', taskKey,
    'failureCount', prevFC + 1,
    'failureTime', now,
    'leaseOwner', '',
    'leaseExpires', leaseExpires,
    'dateModified', now)
redis.call('ZREM', leasedSet, taskID)

-- Only increment failed counter on first failure
if prevFC == 0 then
    redis.call('INCR', failedCtr)
end

return 1
`)

func (s *RedisStore) Fail(ctx context.Context, req *FailRequest) error {
	if req.Permanent {
		_, err := s.archiveTask(ctx, req.TaskID, ResultFailure, 0)
		return err
	}

	retryWait := s.retryWait
	if req.RetryWait != nil {
		retryWait = *req.RetryWait
	}

	now := time.Now().Unix()
	leaseExp := now + int64(retryWait)

	return failScript.Run(ctx, s.rdb,
		[]string{
			s.taskKey(req.TaskID),
			s.unleasedSetKey(),
			s.leasedSetKey(),
			s.failedCounterKey(),
		},
		req.TaskID, now, leaseExp,
	).Err()
}

func (s *RedisStore) Yield(ctx context.Context, taskID int64, duration int) error {
	if duration < 5 {
		duration = 5
	}
	now := time.Now().Unix()
	leaseExp := now + int64(duration)

	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, s.taskKey(taskID), map[string]interface{}{
		"leaseOwner":   YieldOwner,
		"leaseExpires": leaseExp,
		"dateModified": now,
	})
	pipe.ZRem(ctx, s.unleasedSetKey(), taskID)
	pipe.ZAdd(ctx, s.leasedSetKey(), redis.Z{Score: float64(leaseExp), Member: taskID})
	_, err := pipe.Exec(ctx)
	return err
}

var awakenScript = redis.NewScript(`
local leasedSet   = KEYS[1]
local unleasedSet = KEYS[2]
local taskPrefix  = ARGV[1]
local epochAgo    = tonumber(ARGV[2])
local count       = 0
local numIDs      = tonumber(ARGV[3])

for i = 1, numIDs do
    local tid = ARGV[3 + i]
    local tk = taskPrefix .. tid
    local owner = redis.call('HGET', tk, 'leaseOwner')
    local exp   = tonumber(redis.call('HGET', tk, 'leaseExpires') or '0')
    local fc    = tonumber(redis.call('HGET', tk, 'failureCount') or '1')

    if owner == '(yield)' and exp > epochAgo and fc == 0 then
        redis.call('HSET', tk, 'leaseExpires', epochAgo)
        redis.call('ZREM', leasedSet, tid)

        local pri = tonumber(redis.call('HGET', tk, 'priority') or '2000')
        local score = pri * 1e12 + tonumber(tid)
        redis.call('ZADD', unleasedSet, score, tid)
        count = count + 1
    end
end

return count
`)

func (s *RedisStore) Awaken(ctx context.Context, taskIDs []int64) (int64, error) {
	if len(taskIDs) == 0 {
		return 0, nil
	}

	window := int64(3600)
	epochAgo := time.Now().Unix() - window

	args := make([]interface{}, 0, 3+len(taskIDs))
	args = append(args, s.key("task:"), epochAgo, len(taskIDs))
	for _, id := range taskIDs {
		args = append(args, id)
	}

	n, err := awakenScript.Run(ctx, s.rdb,
		[]string{s.leasedSetKey(), s.unleasedSetKey()},
		args...,
	).Int64()
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (s *RedisStore) Stats(ctx context.Context) (*QueueStats, error) {
	pipe := s.rdb.Pipeline()
	activeCmd := pipe.ZCard(ctx, s.activeSetKey())
	leasedCmd := pipe.ZCard(ctx, s.leasedSetKey())
	archivedCmd := pipe.Get(ctx, s.archivedCounterKey())
	failedCmd := pipe.Get(ctx, s.failedCounterKey())
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("stats pipeline: %w", err)
	}

	archivedCount, _ := strconv.ParseInt(archivedCmd.Val(), 10, 64)
	failedCount, _ := strconv.ParseInt(failedCmd.Val(), 10, 64)

	return &QueueStats{
		ActiveCount:   activeCmd.Val(),
		LeasedCount:   leasedCmd.Val(),
		ArchivedCount: archivedCount,
		FailedCount:   failedCount,
	}, nil
}

func (s *RedisStore) GetTask(ctx context.Context, taskID int64) (*Task, error) {
	return s.loadTask(ctx, taskID)
}

func (s *RedisStore) ListActive(ctx context.Context, limit, offset int) ([]*Task, error) {
	if limit <= 0 {
		limit = 100
	}

	ids, err := s.rdb.ZRange(ctx, s.activeSetKey(), int64(offset), int64(offset+limit-1)).Result()
	if err != nil {
		return nil, err
	}

	tasks := make([]*Task, 0, len(ids))
	for _, idStr := range ids {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		t, err := s.loadTask(ctx, id)
		if err != nil {
			return nil, err
		}
		if t != nil {
			tasks = append(tasks, t)
		}
	}
	return tasks, nil
}

func (s *RedisStore) loadTask(ctx context.Context, taskID int64) (*Task, error) {
	vals, err := s.rdb.HGetAll(ctx, s.taskKey(taskID)).Result()
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, nil
	}

	t := mapToTask(vals)

	data, err := s.rdb.Get(ctx, s.dataKey(t.DataID)).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	t.Data = data

	return t, nil
}

func taskToMap(t *Task) map[string]interface{} {
	m := map[string]interface{}{
		"id":            t.ID,
		"taskClass":     t.TaskClass,
		"leaseOwner":    t.LeaseOwner,
		"failureCount":  t.FailureCount,
		"dataID":        t.DataID,
		"priority":      t.Priority,
		"objectPHID":    t.ObjectPHID,
		"containerPHID": t.ContainerPHID,
		"dateCreated":   t.DateCreated,
		"dateModified":  t.DateModified,
	}
	if t.LeaseExpires != nil {
		m["leaseExpires"] = *t.LeaseExpires
	} else {
		m["leaseExpires"] = ""
	}
	if t.FailureTime != nil {
		m["failureTime"] = *t.FailureTime
	} else {
		m["failureTime"] = ""
	}
	return m
}

func mapToTask(m map[string]string) *Task {
	t := &Task{}
	t.ID, _ = strconv.ParseInt(m["id"], 10, 64)
	t.TaskClass = m["taskClass"]
	t.LeaseOwner = m["leaseOwner"]
	t.FailureCount, _ = strconv.Atoi(m["failureCount"])
	t.DataID, _ = strconv.ParseInt(m["dataID"], 10, 64)
	t.Priority, _ = strconv.Atoi(m["priority"])
	t.ObjectPHID = m["objectPHID"]
	t.ContainerPHID = m["containerPHID"]
	t.DateCreated, _ = strconv.ParseInt(m["dateCreated"], 10, 64)
	t.DateModified, _ = strconv.ParseInt(m["dateModified"], 10, 64)

	if v, ok := m["leaseExpires"]; ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			t.LeaseExpires = &n
		}
	}
	if v, ok := m["failureTime"]; ok && v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			t.FailureTime = &n
		}
	}
	return t
}
