package queue

import (
	"strconv"
	"testing"
)

func TestActiveScore(t *testing.T) {
	s1 := activeScore(PriorityAlerts, 1)
	s2 := activeScore(PriorityDefault, 1)
	if s1 >= s2 {
		t.Error("ALERTS priority should produce a lower score than DEFAULT")
	}

	s3 := activeScore(PriorityDefault, 1)
	s4 := activeScore(PriorityDefault, 2)
	if s3 >= s4 {
		t.Error("same priority, lower ID should produce a lower score")
	}
}

func TestTaskToMapAndBack(t *testing.T) {
	leaseExp := int64(9999)
	failTime := int64(1234)
	original := &Task{
		ID:            42,
		TaskClass:     "TestClass",
		LeaseOwner:    "owner1",
		LeaseExpires:  &leaseExp,
		FailureCount:  3,
		DataID:        10,
		FailureTime:   &failTime,
		Priority:      PriorityDefault,
		ObjectPHID:    "PHID-OBJ-123",
		ContainerPHID: "PHID-CNT-456",
		DateCreated:   1000,
		DateModified:  2000,
	}

	m := taskToMap(original)

	strMap := make(map[string]string, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case int64:
			strMap[k] = strconv.FormatInt(val, 10)
		case int:
			strMap[k] = strconv.Itoa(val)
		case string:
			strMap[k] = val
		}
	}

	restored := mapToTask(strMap)

	if restored.ID != original.ID {
		t.Errorf("ID: got %d, want %d", restored.ID, original.ID)
	}
	if restored.TaskClass != original.TaskClass {
		t.Errorf("TaskClass: got %q, want %q", restored.TaskClass, original.TaskClass)
	}
	if restored.LeaseOwner != original.LeaseOwner {
		t.Errorf("LeaseOwner: got %q, want %q", restored.LeaseOwner, original.LeaseOwner)
	}
	if restored.LeaseExpires == nil || *restored.LeaseExpires != *original.LeaseExpires {
		t.Errorf("LeaseExpires mismatch")
	}
	if restored.FailureCount != original.FailureCount {
		t.Errorf("FailureCount: got %d, want %d", restored.FailureCount, original.FailureCount)
	}
	if restored.DataID != original.DataID {
		t.Errorf("DataID: got %d, want %d", restored.DataID, original.DataID)
	}
	if restored.FailureTime == nil || *restored.FailureTime != *original.FailureTime {
		t.Errorf("FailureTime mismatch")
	}
	if restored.Priority != original.Priority {
		t.Errorf("Priority: got %d, want %d", restored.Priority, original.Priority)
	}
	if restored.ObjectPHID != original.ObjectPHID {
		t.Errorf("ObjectPHID: got %q, want %q", restored.ObjectPHID, original.ObjectPHID)
	}
	if restored.ContainerPHID != original.ContainerPHID {
		t.Errorf("ContainerPHID: got %q, want %q", restored.ContainerPHID, original.ContainerPHID)
	}
}

func TestTaskToMapNilPointers(t *testing.T) {
	original := &Task{
		ID:        1,
		TaskClass: "NilTest",
		Priority:  PriorityDefault,
	}

	m := taskToMap(original)

	if m["leaseExpires"] != "" {
		t.Errorf("expected empty string for nil LeaseExpires, got %v", m["leaseExpires"])
	}
	if m["failureTime"] != "" {
		t.Errorf("expected empty string for nil FailureTime, got %v", m["failureTime"])
	}

	strMap := make(map[string]string, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case int64:
			strMap[k] = strconv.FormatInt(val, 10)
		case int:
			strMap[k] = strconv.Itoa(val)
		case string:
			strMap[k] = val
		}
	}

	restored := mapToTask(strMap)
	if restored.LeaseExpires != nil {
		t.Error("expected nil LeaseExpires after round-trip")
	}
	if restored.FailureTime != nil {
		t.Error("expected nil FailureTime after round-trip")
	}
}

func TestRedisStoreKeyMethods(t *testing.T) {
	s := &RedisStore{prefix: "test:"}

	if got := s.taskKey(42); got != "test:task:42" {
		t.Errorf("taskKey(42) = %q, want %q", got, "test:task:42")
	}
	if got := s.dataKey(10); got != "test:data:10" {
		t.Errorf("dataKey(10) = %q, want %q", got, "test:data:10")
	}
	if got := s.archivedKey(5); got != "test:archived:5" {
		t.Errorf("archivedKey(5) = %q, want %q", got, "test:archived:5")
	}
	if got := s.activeSetKey(); got != "test:idx:active" {
		t.Errorf("activeSetKey() = %q, want %q", got, "test:idx:active")
	}
	if got := s.unleasedSetKey(); got != "test:idx:unleased" {
		t.Errorf("unleasedSetKey() = %q, want %q", got, "test:idx:unleased")
	}
	if got := s.leasedSetKey(); got != "test:idx:leased" {
		t.Errorf("leasedSetKey() = %q, want %q", got, "test:idx:leased")
	}
	if got := s.archivedIndexKey(); got != "test:idx:archived" {
		t.Errorf("archivedIndexKey() = %q, want %q", got, "test:idx:archived")
	}
	if got := s.nextTaskIDKey(); got != "test:next_task_id" {
		t.Errorf("nextTaskIDKey() = %q, want %q", got, "test:next_task_id")
	}
	if got := s.nextDataIDKey(); got != "test:next_data_id" {
		t.Errorf("nextDataIDKey() = %q, want %q", got, "test:next_data_id")
	}
}
