package queue

import "context"

type Store interface {
	Enqueue(ctx context.Context, req *EnqueueRequest) (*Task, error)
	Lease(ctx context.Context, limit int, leaseOwner string) ([]*Task, error)
	Complete(ctx context.Context, taskID int64, duration int64) (*ArchivedTask, error)
	Fail(ctx context.Context, req *FailRequest) error
	Yield(ctx context.Context, taskID int64, duration int) error
	Cancel(ctx context.Context, taskID int64) (*ArchivedTask, error)
	Awaken(ctx context.Context, taskIDs []int64) (int64, error)
	Stats(ctx context.Context) (*QueueStats, error)
	GetTask(ctx context.Context, taskID int64) (*Task, error)
	ListActive(ctx context.Context, limit, offset int) ([]*Task, error)
	Close() error
}
