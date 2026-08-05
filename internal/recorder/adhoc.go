package recorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// MaxAdhocDuration caps an open-ended "record now" job. Without it a
// forgotten recording fills the disk; 12h is longer than any single
// broadcast and short enough to bound the damage.
const MaxAdhocDuration = 12 * time.Hour

// startTimeout bounds how long Start waits for the recording row to be
// created. Run writes the row before touching hardware, so this only
// trips on a wedged sqlite.
const startTimeout = 5 * time.Second

// ErrNotRecording is returned when Stop names a job that isn't running
// (already finished, already stopped, or never existed).
var ErrNotRecording = errors.New("recorder: not an active recording")

// Manager owns "record now" jobs — the ones started by a person
// pressing a button rather than by the scheduler. It hands back the
// recording row id so the job can be stopped later, and keeps a
// registry of what's currently rolling.
//
// Scheduled recordings do not go through here; the scheduler drives
// Runner directly and its jobs end on their own EPG-derived deadline.
type Manager struct {
	Runner *Runner

	// Base is the daemon lifetime context. Jobs are children of it, not
	// of the HTTP request that started them, so a recording outlives the
	// POST that asked for it. Nil means context.Background().
	Base context.Context

	mu     sync.Mutex
	active map[int64]*adhocJob
}

type adhocJob struct {
	channel string
	title   string
	stop    chan struct{}
	done    chan struct{} // closed when Run has returned and the row is final
	once    sync.Once
	cancel  context.CancelFunc
}

func (j *adhocJob) signalStop() { j.once.Do(func() { close(j.stop) }) }

// Start begins recording channel immediately and returns the recording
// row id. dur <= 0 means open-ended (capped at MaxAdhocDuration) —
// stop it with Stop.
//
// Start returns as soon as the row exists; it does not wait for the
// tuner. A tune failure (including "tuner busy") lands in the row's
// state/error, so callers should surface the row rather than assume a
// 2xx means bytes are flowing. Use tuner.Pool.CanServe for a cheap
// upfront rejection.
func (m *Manager) Start(ctx context.Context, channel, title string, dur time.Duration) (int64, error) {
	if m.Runner == nil {
		return 0, errors.New("recorder: Manager.Runner is nil")
	}
	if channel == "" {
		return 0, errors.New("recorder: channel required")
	}
	if dur <= 0 || dur > MaxAdhocDuration {
		if dur > MaxAdhocDuration {
			slog.Warn("recorder: ad-hoc duration capped",
				"channel", channel, "asked", dur, "cap", MaxAdhocDuration)
		}
		dur = MaxAdhocDuration
	}

	base := m.Base
	if base == nil {
		base = context.Background()
	}
	jobCtx, cancel := context.WithCancel(base)

	now := time.Now()
	job := &adhocJob{
		channel: channel,
		title:   title,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		cancel:  cancel,
	}

	idCh := make(chan int64, 1)
	errCh := make(chan error, 1)

	j := Job{
		Channel: channel,
		Title:   title,
		Start:   now,
		End:     now.Add(dur),
		Stop:    job.stop,
		OnStart: func(id int64) {
			// Register before Run can possibly finish, so the deregister
			// below never races ahead of the insert.
			m.mu.Lock()
			if m.active == nil {
				m.active = make(map[int64]*adhocJob)
			}
			m.active[id] = job
			m.mu.Unlock()
			idCh <- id
		},
	}

	go func() {
		defer cancel()
		defer close(job.done)
		err := m.Runner.Run(jobCtx, j)
		m.deregister(job)
		if err != nil {
			slog.Warn("recorder: ad-hoc recording ended with error",
				"channel", channel, "err", err)
		}
		errCh <- err
	}()

	select {
	case id := <-idCh:
		slog.Info("recorder: ad-hoc recording started",
			"id", id, "channel", channel, "title", title, "max", dur)
		return id, nil
	case err := <-errCh:
		// The job can fail fast (busy tuner) with the row already
		// written. Prefer the id so the outcome is deterministic: the
		// caller gets a row to display, carrying state='failed'.
		select {
		case id := <-idCh:
			return id, nil
		default:
		}
		if err == nil {
			err = errors.New("recorder: job ended before reporting an id")
		}
		return 0, err
	case <-time.After(startTimeout):
		cancel()
		return 0, fmt.Errorf("recorder: no recording row within %s", startTimeout)
	case <-ctx.Done():
		cancel()
		return 0, ctx.Err()
	}
}

// Stop ends an ad-hoc recording gracefully: the row finalizes as
// 'done' with whatever was written. Returns ErrNotRecording when id
// isn't active.
func (m *Manager) Stop(id int64) error {
	m.mu.Lock()
	job, ok := m.active[id]
	m.mu.Unlock()
	if !ok {
		return ErrNotRecording
	}
	slog.Info("recorder: stopping ad-hoc recording", "id", id, "channel", job.channel)
	job.signalStop()
	return nil
}

// StopAll gracefully ends every ad-hoc recording, without waiting for
// the rows to be written. Prefer StopAllAndWait on the shutdown path.
func (m *Manager) StopAll() {
	for _, job := range m.snapshot() {
		job.signalStop()
	}
}

// StopAllAndWait stops every ad-hoc recording and waits (up to timeout)
// for each one's row to be finalized. Returns the number still running
// when it gave up — non-zero means those rows are stuck in state
// 'recording'.
//
// The daemon must call this *before* closing the store: signalling a
// stop only starts the teardown, and a recorder mid-finalize against a
// closed database leaves the row looking like it never ended.
func (m *Manager) StopAllAndWait(timeout time.Duration) int {
	jobs := m.snapshot()
	for _, job := range jobs {
		job.signalStop()
	}

	deadline := time.After(timeout)
	stuck := 0
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-deadline:
			stuck++
		}
	}
	if stuck > 0 {
		slog.Warn("recorder: recordings did not finalize before shutdown",
			"count", stuck, "waited", timeout)
	}
	return stuck
}

func (m *Manager) snapshot() []*adhocJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	jobs := make([]*adhocJob, 0, len(m.active))
	for _, job := range m.active {
		jobs = append(jobs, job)
	}
	return jobs
}

// Active lists the recording ids currently rolling, ascending.
func (m *Manager) Active() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]int64, 0, len(m.active))
	for id := range m.active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, k int) bool { return ids[i] < ids[k] })
	return ids
}

// deregister drops job from the registry by identity — Start owns the
// id channel, so the job goroutine matches on the pointer instead.
func (m *Manager) deregister(job *adhocJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, v := range m.active {
		if v == job {
			delete(m.active, id)
			return
		}
	}
}
