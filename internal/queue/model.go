package queue

const (
	ResultSuccess   = 0
	ResultFailure   = 1
	ResultCancelled = 2
)

const (
	PriorityAlerts  = 1000
	PriorityDefault = 2000
	PriorityCommit  = 2500
	PriorityBulk    = 3000
	PriorityIndex   = 3500
	PriorityImport  = 4000
)

const YieldOwner = "(yield)"

type Task struct {
	ID            int64  `json:"id"`
	TaskClass     string `json:"taskClass"`
	LeaseOwner    string `json:"leaseOwner,omitempty"`
	LeaseExpires  *int64 `json:"leaseExpires,omitempty"`
	FailureCount  int    `json:"failureCount"`
	DataID        int64  `json:"dataID"`
	FailureTime   *int64 `json:"failureTime,omitempty"`
	Priority      int    `json:"priority"`
	ObjectPHID    string `json:"objectPHID,omitempty"`
	ContainerPHID string `json:"containerPHID,omitempty"`
	DateCreated   int64  `json:"dateCreated"`
	DateModified  int64  `json:"dateModified"`
	Data          string `json:"data,omitempty"`
}

type ArchivedTask struct {
	Task
	Result        int    `json:"result"`
	Duration      int64  `json:"duration"`
	ArchivedEpoch *int64 `json:"archivedEpoch,omitempty"`
}

type EnqueueRequest struct {
	TaskClass     string `json:"taskClass"`
	Data          string `json:"data"`
	Priority      *int   `json:"priority,omitempty"`
	ObjectPHID    string `json:"objectPHID,omitempty"`
	ContainerPHID string `json:"containerPHID,omitempty"`
	DelayUntil    *int64 `json:"delayUntil,omitempty"`
}

type LeaseRequest struct {
	Limit int `json:"limit"`
}

type CompleteRequest struct {
	TaskID   int64 `json:"taskID"`
	Duration int64 `json:"duration"`
}

type FailRequest struct {
	TaskID    int64  `json:"taskID"`
	Permanent bool   `json:"permanent"`
	RetryWait *int   `json:"retryWait,omitempty"`
	Message   string `json:"message,omitempty"`
}

type YieldRequest struct {
	TaskID   int64 `json:"taskID"`
	Duration int   `json:"duration"`
}

type AwakenRequest struct {
	TaskIDs []int64 `json:"taskIDs"`
}

type CancelRequest struct {
	TaskID int64 `json:"taskID"`
}

type QueueStats struct {
	ActiveCount   int64 `json:"activeCount"`
	LeasedCount   int64 `json:"leasedCount"`
	ArchivedCount int64 `json:"archivedCount"`
	FailedCount   int64 `json:"failedCount"`
}
