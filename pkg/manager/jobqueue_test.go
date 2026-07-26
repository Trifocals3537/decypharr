package manager

import (
	"sync"
	"testing"
)

func TestJobQueuePopClearsReferencesAndReusesStorage(t *testing.T) {
	q := &JobQueue{jobs: make([]*Job, 0, 2)}
	q.cond = sync.NewCond(&q.mu)
	first := &Job{ID: "first"}
	second := &Job{ID: "second"}
	q.jobs = append(q.jobs, first, second)
	backing := q.jobs[:cap(q.jobs)]

	if got := q.pop(); got != first {
		t.Fatalf("first pop = %p, want %p", got, first)
	}
	if backing[0] != nil {
		t.Fatal("first consumed job is still retained by the queue backing array")
	}

	if got := q.pop(); got != second {
		t.Fatalf("second pop = %p, want %p", got, second)
	}
	if len(q.jobs) != 0 || q.head != 0 {
		t.Fatalf("drained queue state = len %d, head %d; want zero values", len(q.jobs), q.head)
	}

	q.jobs = append(q.jobs, &Job{ID: "third"})
	if &q.jobs[:cap(q.jobs)][0] != &backing[0] {
		t.Fatal("drained queue did not reuse its backing array")
	}
}

func TestJobQueueDeleteClearsTailAfterConsumedPrefix(t *testing.T) {
	q := &JobQueue{jobs: make([]*Job, 0, 3)}
	q.cond = sync.NewCond(&q.mu)
	first := &Job{ID: "first", Type: JobTypeTorrent}
	second := &Job{ID: "second", Type: JobTypeNZB}
	third := &Job{ID: "third", Type: JobTypeTorrent}
	q.jobs = append(q.jobs, first, second, third)
	backing := q.jobs[:cap(q.jobs)]

	if got := q.pop(); got != first {
		t.Fatalf("pop = %p, want %p", got, first)
	}
	if !q.DeleteJob(second.ID) {
		t.Fatal("DeleteJob(second) = false, want true")
	}
	if backing[2] != nil {
		t.Fatal("deleted job left a stale pointer at the queue tail")
	}
	if got := q.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
	if got := q.FindJob(third.ID); got != third {
		t.Fatalf("FindJob(third) = %p, want %p", got, third)
	}
	if got := q.PendingCount(JobTypeTorrent); got != 1 {
		t.Fatalf("PendingCount(torrent) = %d, want 1", got)
	}
	if got := q.PendingCount(JobTypeNZB); got != 0 {
		t.Fatalf("PendingCount(nzb) = %d, want 0", got)
	}
}

func BenchmarkJobQueueBatchReuse(b *testing.B) {
	const batchSize = 64
	q := &JobQueue{jobs: make([]*Job, 0, batchSize)}
	q.cond = sync.NewCond(&q.mu)

	jobs := make([]*Job, batchSize)
	for i := range jobs {
		jobs[i] = &Job{ID: "benchmark"}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		q.mu.Lock()
		q.jobs = append(q.jobs, jobs...)
		q.mu.Unlock()

		for range batchSize {
			if job := q.pop(); job == nil {
				b.Fatal("pop returned nil")
			}
		}
	}
}
