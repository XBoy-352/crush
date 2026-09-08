package childjobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testJob(sessionID string) *Job {
	return &Job{Kind: KindAgent, ParentSessionID: sessionID, Title: "test"}
}

func TestStartRunsWithoutParentCancel(t *testing.T) {
	t.Parallel()
	r := GetRegistry()

	parentCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	job := testJob(t.Name())
	err := r.Start(parentCtx, job, func(ctx context.Context) (string, error) {
		<-ctx.Done() // would return immediately if the parent cancel propagated
		cancel()
		return "", ctx.Err()
	}, func(j *Job) { close(done) })
	require.NoError(t, err)

	cancel()
	select {
	case <-done:
		t.Fatal("job must survive parent cancellation")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, r.Kill(job.ID))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onDone not called after kill")
	}
}

func TestStartDone(t *testing.T) {
	t.Parallel()
	r := GetRegistry()
	job := testJob(t.Name())
	var fired atomic.Int32
	err := r.Start(context.Background(), job, func(ctx context.Context) (string, error) {
		return "the result", nil
	}, func(j *Job) {
		fired.Add(1)
	})
	require.NoError(t, err)
	require.Equal(t, StatusRunning, job.Status())
	require.True(t, strings.HasPrefix(job.ID, "agent-"))

	require.Eventually(t, func() bool {
		got, ok := r.Get(job.ID)
		return ok && got.Status() == StatusDone
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, int32(1), fired.Load())
	got, _ := r.Get(job.ID)
	require.Equal(t, "the result", got.Result())
	require.True(t, got.Done())
}

func TestStartError(t *testing.T) {
	t.Parallel()
	r := GetRegistry()
	job := testJob(t.Name())
	err := r.Start(context.Background(), job, func(ctx context.Context) (string, error) {
		return "", errors.New("boom")
	}, nil)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, _ := r.Get(job.ID)
		return got.Status() == StatusError
	}, time.Second, 5*time.Millisecond)
	got, _ := r.Get(job.ID)
	require.Equal(t, "boom", got.Err())
}

func TestKillRunning(t *testing.T) {
	t.Parallel()
	r := GetRegistry()
	job := testJob(t.Name())
	started := make(chan struct{})
	var fired atomic.Int32
	err := r.Start(context.Background(), job, func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}, func(j *Job) { fired.Add(1) })
	require.NoError(t, err)
	<-started

	require.NoError(t, r.Kill(job.ID))
	require.Eventually(t, func() bool {
		got, _ := r.Get(job.ID)
		return got.Status() == StatusKilled
	}, time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return fired.Load() == 1 }, time.Second, 5*time.Millisecond)

	require.ErrorIs(t, r.Kill(job.ID), ErrNotFound)
}

func TestKillQueuedNeverStarts(t *testing.T) {
	t.Parallel()
	r := GetRegistry()

	first := testJob(t.Name())
	require.NoError(t, r.Start(context.Background(), first, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, nil))

	// Session capacity is 5, so fill the remaining slots, leaving this one queued.
	var queued *Job
	for i := 0; i < DefaultMaxConcurrentPerSession; i++ {
		j := testJob(t.Name())
		require.NoError(t, r.Start(context.Background(), j, func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, nil))
		queued = j
	}
	require.Equal(t, StatusQueued, queued.Status())

	require.NoError(t, r.Kill(queued.ID))
	require.Equal(t, StatusKilled, queued.Status())
	require.Equal(t, "killed before start", queued.Err())

	// Give any wrongly-started goroutine a chance to run.
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, StatusKilled, queued.Status())
}

func TestSemaphoreQueuesUntilSlotFrees(t *testing.T) {
	t.Parallel()
	r := GetRegistry()

	var running atomic.Int32
	var maxRunning atomic.Int32
	var mu sync.Mutex
	completed := 0

	start := func() *Job {
		j := testJob(t.Name())
		require.NoError(t, r.Start(context.Background(), j, func(ctx context.Context) (string, error) {
			n := running.Add(1)
			for {
				m := maxRunning.Load()
				if n <= m || maxRunning.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			running.Add(-1)
			return "ok", nil
		}, func(j *Job) {
			mu.Lock()
			completed++
			mu.Unlock()
		}))
		return j
	}

	jobs := make([]*Job, 0, DefaultMaxConcurrentPerSession+3)
	for i := 0; i < DefaultMaxConcurrentPerSession+3; i++ {
		jobs = append(jobs, start())
	}
	require.Equal(t, StatusQueued, jobs[DefaultMaxConcurrentPerSession].Status())

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return completed == DefaultMaxConcurrentPerSession+3
	}, 5*time.Second, 10*time.Millisecond)
	require.LessOrEqual(t, maxRunning.Load(), int32(DefaultMaxConcurrentPerSession))

	for _, j := range jobs {
		got, _ := r.Get(j.ID)
		require.Equal(t, StatusDone, got.Status())
	}
}

func TestHardCapRejects(t *testing.T) {
	t.Parallel()
	r := GetRegistry()
	for i := 0; i < HardMaxConcurrentPerSession; i++ {
		j := testJob(t.Name())
		err := r.Start(context.Background(), j, func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, nil)
		if i < DefaultMaxConcurrentPerSession {
			require.NoError(t, err)
		} else {
			require.Equal(t, StatusQueued, j.Status())
		}
	}
	err := r.Start(context.Background(), testJob(t.Name()), func(ctx context.Context) (string, error) {
		return "", nil
	}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "hard limit")
}

func TestOnDoneExactlyOnceUnderKill(t *testing.T) {
	t.Parallel()
	r := GetRegistry()
	job := testJob(t.Name())
	started := make(chan struct{})
	var fired atomic.Int32
	require.NoError(t, r.Start(context.Background(), job, func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "late result", nil // run succeeds even though ctx was killed
	}, func(j *Job) { fired.Add(1) }))
	<-started

	require.NoError(t, r.Kill(job.ID))
	require.Eventually(t, func() bool { return fired.Load() == 1 }, time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(1), fired.Load())
	got, _ := r.Get(job.ID)
	require.Equal(t, StatusKilled, got.Status())
}

func TestWorkflowKillCascade(t *testing.T) {
	t.Parallel()
	r := GetRegistry()

	root := &Job{Kind: KindWorkflow, ParentSessionID: "s1", ToolCallID: "call-9", Title: "wf"}
	workerCtxs := make(chan context.CancelFunc, 4)
	var workers []*Job
	require.NoError(t, r.Start(context.Background(), root, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, nil))

	for i := 0; i < 3; i++ {
		w := &Job{
			Kind: KindWorkflowWorker, ParentSessionID: "s1",
			ToolCallID: "call-9-a" + string(rune('0'+i)), Title: "worker",
		}
		err := r.StartDetached(context.Background(), w, func(ctx context.Context) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}, nil)
		require.NoError(t, err)
		workers = append(workers, w)
	}
	_ = workerCtxs

	// An unrelated worker must not be touched.
	unrelated := &Job{Kind: KindWorkflowWorker, ParentSessionID: "s1", ToolCallID: "other-call-a0"}
	require.NoError(t, r.StartDetached(context.Background(), unrelated, func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, nil))

	require.NoError(t, r.Kill(root.ID))
	require.Eventually(t, func() bool {
		for _, w := range workers {
			if w.Status() != StatusKilled {
				return false
			}
		}
		return root.Status() == StatusKilled
	}, time.Second, 5*time.Millisecond)
	require.Equal(t, StatusRunning, unrelated.Status())

	require.NoError(t, r.Kill(unrelated.ID))
}

func TestListAndListByParent(t *testing.T) {
	t.Parallel()
	r := GetRegistry()
	for _, sess := range []string{t.Name() + "-a", t.Name() + "-b", t.Name() + "-a"} {
		j := testJob(sess)
		require.NoError(t, r.Start(context.Background(), j, func(ctx context.Context) (string, error) {
			return "ok", nil
		}, nil))
	}
	require.Eventually(t, func() bool {
		return len(r.ListByParent(t.Name()+"-a")) == 2 && len(r.ListByParent(t.Name()+"-b")) == 1
	}, time.Second, 5*time.Millisecond)
	require.GreaterOrEqual(t, len(r.List()), 3)

	list := r.ListByParent(t.Name() + "-a")
	require.Len(t, list, 2)
	require.True(t, list[0].StartedAt.Before(list[1].StartedAt) || list[0].StartedAt.Equal(list[1].StartedAt))
}
