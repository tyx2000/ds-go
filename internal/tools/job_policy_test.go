package tools

import (
	"testing"
	"time"
)

func TestConfiguredJobWaitIsLocalAndClampsModelTimeout(t *testing.T) {
	r := New()
	defer r.Close()
	if err := r.SetJobWait(12, 40); err != nil {
		t.Fatal(err)
	}
	if wait, err := r.JobWait(nil); err != nil || wait != 12*time.Millisecond {
		t.Fatalf("configured default: %v %v", wait, err)
	}
	requested := 999.0
	if wait, err := r.JobWait(&requested); err != nil || wait != 40*time.Millisecond {
		t.Fatalf("configured cap: %v %v", wait, err)
	}
	other := New()
	defer other.Close()
	if wait, err := other.JobWait(nil); err != nil || wait != 30*time.Second {
		t.Fatalf("configuration leaked: %v %v", wait, err)
	}
	if err := r.SetJobWait(50, 20); err == nil {
		t.Fatal("default greater than cap accepted")
	}
}
