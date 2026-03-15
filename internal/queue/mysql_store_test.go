package queue

import (
	"testing"
)

func TestPlaceholders(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, ""},
		{1, "?"},
		{3, "?,?,?"},
		{5, "?,?,?,?,?"},
	}
	for _, tt := range tests {
		got := placeholders(tt.n)
		if got != tt.want {
			t.Errorf("placeholders(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestAppendArgs(t *testing.T) {
	got := appendArgs("owner", int64(100), []int64{1, 2, 3})
	if len(got) != 5 {
		t.Errorf("expected 5 args, got %d", len(got))
	}
	if got[0] != "owner" {
		t.Errorf("expected first arg 'owner', got %v", got[0])
	}
	if got[1] != int64(100) {
		t.Errorf("expected second arg 100, got %v", got[1])
	}
}

func TestNullStr(t *testing.T) {
	if nullStr("") != nil {
		t.Error("expected nil for empty string")
	}
	if nullStr("hello") != "hello" {
		t.Error("expected 'hello' for non-empty string")
	}
}

func TestConstants(t *testing.T) {
	if PriorityAlerts >= PriorityDefault {
		t.Error("ALERTS should be higher priority (lower number) than DEFAULT")
	}
	if PriorityDefault >= PriorityCommit {
		t.Error("DEFAULT should be higher priority than COMMIT")
	}
	if PriorityCommit >= PriorityBulk {
		t.Error("COMMIT should be higher priority than BULK")
	}
	if PriorityBulk >= PriorityIndex {
		t.Error("BULK should be higher priority than INDEX")
	}
	if PriorityIndex >= PriorityImport {
		t.Error("INDEX should be higher priority than IMPORT")
	}

	if ResultSuccess != 0 {
		t.Errorf("ResultSuccess should be 0, got %d", ResultSuccess)
	}
	if ResultFailure != 1 {
		t.Errorf("ResultFailure should be 1, got %d", ResultFailure)
	}
	if ResultCancelled != 2 {
		t.Errorf("ResultCancelled should be 2, got %d", ResultCancelled)
	}
}
