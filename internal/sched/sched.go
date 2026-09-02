// Package sched runs background tasks on fixed intervals.
//
// The cabinet had no background work at all: every number was fetched
// while a page was being rendered. That is fine for a dashboard and fatal
// for accounting, because ESI serves wallets, contracts and jobs through a
// rolling window and then forgets them (ACCOUNTING.md §1).
//
// Deliberately small: one goroutine per task, a ticker, and a context that
// stops all of them. No cron, no persistence — a task that missed its slot
// simply runs at the next tick, and every collector is idempotent.
package sched

import (
	"context"
	"log"
	"sync"
	"time"
)

type Task struct {
	Name  string
	Every time.Duration
	// First delays the initial run. Tasks are staggered so a restart does
	// not fire every collector at once against the same ESI error budget.
	First time.Duration
	Run   func(context.Context) error
}

type Scheduler struct {
	tasks  []Task
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func New() *Scheduler { return &Scheduler{} }

func (s *Scheduler) Add(t Task) { s.tasks = append(s.tasks, t) }

// Start launches every task. It returns immediately.
func (s *Scheduler) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	for _, t := range s.tasks {
		s.wg.Add(1)
		go s.loop(ctx, t)
	}
	log.Printf("планировщик: запущено задач — %d", len(s.tasks))
}

func (s *Scheduler) loop(ctx context.Context, t Task) {
	defer s.wg.Done()

	if t.First > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(t.First):
		}
	}
	run := func() {
		started := time.Now()
		if err := t.Run(ctx); err != nil {
			log.Printf("задача %s: %v (%s)", t.Name, err, time.Since(started).Round(time.Millisecond))
			return
		}
		log.Printf("задача %s: готово за %s", t.Name, time.Since(started).Round(time.Millisecond))
	}
	run()

	tick := time.NewTicker(t.Every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			run()
		}
	}
}

// Stop cancels every task and waits for the running ones to return.
func (s *Scheduler) Stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
	log.Print("планировщик: остановлен")
}
