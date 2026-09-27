package btserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadRecordsMarksInterrupted 验证服务重启后,上次遗留的 queued/running 作业
// 被标记为失败(而不是永远显示运行中)。
func TestLoadRecordsMarksInterrupted(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(nil, Config{RecordsDir: dir, MaxConcurrent: 1})

	// 构造两条"上次进程遗留"的记录:running 与 queued
	for _, job := range []*Job{
		{ID: "job-running", Status: "running", StrategyName: "a", CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "job-queued", Status: "queued", StrategyName: "b", CreatedAt: time.Now().Format(time.RFC3339)},
		{ID: "job-done", Status: "done", StrategyName: "c", CreatedAt: time.Now().Format(time.RFC3339)},
	} {
		raw, _ := json.Marshal(job)
		if err := os.WriteFile(filepath.Join(dir, job.ID+".json"), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	srv.LoadRecords()

	for _, id := range []string{"job-running", "job-queued"} {
		job := srv.loadRecord(id)
		if job == nil {
			t.Fatalf("%s: record missing", id)
		}
		if job.Status != "failed" {
			t.Fatalf("%s: status=%s, want failed", id, job.Status)
		}
		if job.Error == "" {
			t.Fatalf("%s: error message empty", id)
		}
	}
	if done := srv.loadRecord("job-done"); done == nil || done.Status != "done" {
		t.Fatalf("done record should stay done: %+v", done)
	}
}

// TestInterruptRunningJobs 验证停机时把在途作业标记为失败并落盘。
func TestInterruptRunningJobs(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(nil, Config{RecordsDir: dir, MaxConcurrent: 1})
	srv.jobs["job-1"] = &Job{ID: "job-1", Status: "running", StrategyName: "x"}
	srv.jobs["job-2"] = &Job{ID: "job-2", Status: "done", StrategyName: "y"}

	srv.InterruptRunningJobs("服务重启,作业被中断")

	job := srv.jobs["job-1"]
	if job.Status != "failed" || job.Error == "" {
		t.Fatalf("job-1 not interrupted: %+v", job)
	}
	if srv.jobs["job-2"].Status != "done" {
		t.Fatalf("job-2 should stay done")
	}
	// 落盘可读回
	if saved := srv.loadRecord("job-1"); saved == nil || saved.Status != "failed" {
		t.Fatalf("job-1 not persisted: %+v", saved)
	}
}
