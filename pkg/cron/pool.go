package cron

import (
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/cryptoquantumwave/khunquant/pkg/logger"
)

// Default sizing for the job worker pool and the Options zero-value fallbacks.
const (
	defaultWorkers      = 4
	defaultStopTimeout  = 10 * time.Second
	minPoolQueueSize    = 64
	poolQueueMultiplier = 16
)

// workerPool is a fixed set of goroutines draining a bounded queue of job IDs.
//
// The ticker goroutine only ever calls trySubmit, which never blocks, so a slow
// job can delay neither the ticker nor the other workers. A job ID is queued at
// most once at a time because the caller claims the job (State.Running) before
// submitting it and the flag is only cleared when the run finishes.
type workerPool struct {
	queue chan string
	wg    sync.WaitGroup
}

func newWorkerPool(workers, queueSize int, run func(jobID string)) *workerPool {
	if workers <= 0 {
		workers = defaultWorkers
	}
	if queueSize < minPoolQueueSize {
		queueSize = minPoolQueueSize
	}
	p := &workerPool{queue: make(chan string, queueSize)}
	p.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer p.wg.Done()
			for id := range p.queue {
				safeRun(run, id)
			}
		}()
	}
	return p
}

// safeRun keeps a worker alive if run itself panics (executeJobByID recovers
// handler panics, so this only guards the bookkeeping around it).
func safeRun(run func(jobID string), id string) {
	defer func() {
		if r := recover(); r != nil {
			logger.ErrorCF("cron", "worker panic outside job handler", map[string]any{
				"job_id": id,
				"panic":  fmt.Sprint(r),
				"stack":  string(debug.Stack()),
			})
		}
	}()
	run(id)
}

// trySubmit queues jobID without blocking. It reports false when the queue is full.
// It must not be called after shutdown.
func (p *workerPool) trySubmit(jobID string) bool {
	select {
	case p.queue <- jobID:
		return true
	default:
		return false
	}
}

// shutdown closes the queue, lets the workers drain what is already claimed and
// waits up to timeout (<=0 waits indefinitely). It reports whether every worker
// finished in time.
func (p *workerPool) shutdown(timeout time.Duration) bool {
	close(p.queue)
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
