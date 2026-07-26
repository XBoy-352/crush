package shell

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/crush/internal/csync"
)

const (
	// MaxBackgroundJobs is the maximum number of concurrent background jobs allowed
	MaxBackgroundJobs = 50
	// CompletedJobRetentionMinutes is how long to keep completed jobs before auto-cleanup (8 hours)
	CompletedJobRetentionMinutes = 8 * 60

	defaultSyncBufferHeadBytes = 1 << 20 // 1 MiB
	defaultSyncBufferTailBytes = 1 << 20 // 1 MiB
)

// syncBuffer is a thread-safe output buffer that retains a fixed head and a
// rolling tail once the stream exceeds head+tail bytes. A single large Write
// never retains more than head+tail bytes of payload.
type syncBuffer struct {
	mu        sync.RWMutex
	headLimit int
	tailLimit int
	head      []byte
	tail      []byte
	total     int64
	truncated bool
}

func newSyncBuffer() *syncBuffer {
	return &syncBuffer{
		headLimit: defaultSyncBufferHeadBytes,
		tailLimit: defaultSyncBufferTailBytes,
	}
}

func (sb *syncBuffer) Write(p []byte) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.writeLocked(p)
	return len(p), nil
}

func (sb *syncBuffer) WriteString(s string) (n int, err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	sb.writeLocked([]byte(s))
	return len(s), nil
}

func (sb *syncBuffer) writeLocked(p []byte) {
	if len(p) == 0 {
		return
	}
	sb.total += int64(len(p))

	if len(sb.head) < sb.headLimit {
		need := sb.headLimit - len(sb.head)
		if len(p) <= need {
			sb.head = append(sb.head, p...)
			return
		}
		sb.head = append(sb.head, p[:need]...)
		p = p[need:]
	}
	if len(p) == 0 {
		return
	}

	// Keep only the last tailLimit bytes of everything past the head.
	// Never allocate more than tailLimit for the incoming slice.
	if len(p) >= sb.tailLimit {
		// Prior tail content (if any) is fully superseded by this write.
		if len(sb.tail) > 0 {
			sb.truncated = true
		}
		sb.tail = append(sb.tail[:0], p[len(p)-sb.tailLimit:]...)
	} else {
		sb.tail = append(sb.tail, p...)
		if len(sb.tail) > sb.tailLimit {
			sb.tail = append([]byte(nil), sb.tail[len(sb.tail)-sb.tailLimit:]...)
			sb.truncated = true
		}
	}
	// Marker only when the retained window is smaller than total written.
	if sb.total > int64(len(sb.head)+len(sb.tail)) {
		sb.truncated = true
	}
}

func (sb *syncBuffer) String() string {
	sb.mu.RLock()
	defer sb.mu.RUnlock()
	if !sb.truncated {
		if len(sb.tail) == 0 {
			return string(sb.head)
		}
		var b strings.Builder
		b.Grow(len(sb.head) + len(sb.tail))
		b.Write(sb.head)
		b.Write(sb.tail)
		return b.String()
	}
	omitted := sb.total - int64(len(sb.head)) - int64(len(sb.tail))
	if omitted < 0 {
		omitted = 0
	}
	var b strings.Builder
	b.Grow(len(sb.head) + len(sb.tail) + 64)
	b.Write(sb.head)
	fmt.Fprintf(&b, "\n\n... [%d bytes truncated] ...\n\n", omitted)
	b.Write(sb.tail)
	return b.String()
}

// BackgroundShell represents a shell running in the background.
type BackgroundShell struct {
	ID          string
	Command     string
	Description string
	Shell       *Shell
	WorkingDir  string
	ctx         context.Context
	cancel      context.CancelFunc
	stdout      *syncBuffer
	stderr      *syncBuffer
	done        chan struct{}
	exitErr     error
	completedAt atomic.Int64 // Unix timestamp when job completed (0 if still running)
}

// BackgroundShellManager manages background shell instances.
type BackgroundShellManager struct {
	shells *csync.Map[string, *BackgroundShell]
}

var (
	backgroundManager     *BackgroundShellManager
	backgroundManagerOnce sync.Once
	idCounter             atomic.Uint64
)

// newBackgroundShellManager creates a new BackgroundShellManager instance.
func newBackgroundShellManager() *BackgroundShellManager {
	return &BackgroundShellManager{
		shells: csync.NewMap[string, *BackgroundShell](),
	}
}

// GetBackgroundShellManager returns the singleton background shell manager.
func GetBackgroundShellManager() *BackgroundShellManager {
	backgroundManagerOnce.Do(func() {
		backgroundManager = newBackgroundShellManager()
	})
	return backgroundManager
}

// Start creates and starts a new background shell with the given command.
func (m *BackgroundShellManager) Start(ctx context.Context, workingDir string, blockFuncs []BlockFunc, command string, description string) (*BackgroundShell, error) {
	// Check job limit
	if m.shells.Len() >= MaxBackgroundJobs {
		return nil, fmt.Errorf("maximum number of background jobs (%d) reached. Please terminate or wait for some jobs to complete", MaxBackgroundJobs)
	}

	id := fmt.Sprintf("%03X", idCounter.Add(1))

	shell := NewShell(&Options{
		WorkingDir: workingDir,
		BlockFuncs: blockFuncs,
	})

	shellCtx, cancel := context.WithCancel(ctx)

	bgShell := &BackgroundShell{
		ID:          id,
		Command:     command,
		Description: description,
		WorkingDir:  workingDir,
		Shell:       shell,
		ctx:         shellCtx,
		cancel:      cancel,
		stdout:      newSyncBuffer(),
		stderr:      newSyncBuffer(),
		done:        make(chan struct{}),
	}

	m.shells.Set(id, bgShell)

	go func() {
		defer close(bgShell.done)

		err := shell.ExecStream(shellCtx, command, bgShell.stdout, bgShell.stderr)

		bgShell.exitErr = err
		bgShell.completedAt.Store(time.Now().Unix())
	}()

	return bgShell, nil
}

// Get retrieves a background shell by ID.
func (m *BackgroundShellManager) Get(id string) (*BackgroundShell, bool) {
	return m.shells.Get(id)
}

// Remove removes a background shell from the manager without terminating it.
// This is useful when a shell has already completed and you just want to clean up tracking.
func (m *BackgroundShellManager) Remove(id string) error {
	_, ok := m.shells.Take(id)
	if !ok {
		return fmt.Errorf("background shell not found: %s", id)
	}
	return nil
}

// Kill terminates a background shell by ID.
func (m *BackgroundShellManager) Kill(id string) error {
	shell, ok := m.shells.Take(id)
	if !ok {
		return fmt.Errorf("background shell not found: %s", id)
	}

	shell.cancel()
	<-shell.done
	return nil
}

// BackgroundShellInfo contains information about a background shell.
type BackgroundShellInfo struct {
	ID          string
	Command     string
	Description string
}

// List returns all background shell IDs.
func (m *BackgroundShellManager) List() []string {
	ids := make([]string, 0, m.shells.Len())
	for id := range m.shells.Seq2() {
		ids = append(ids, id)
	}
	return ids
}

// Cleanup removes completed jobs that have been finished for more than the retention period
func (m *BackgroundShellManager) Cleanup() int {
	now := time.Now().Unix()
	retentionSeconds := int64(CompletedJobRetentionMinutes * 60)

	var toRemove []string
	for shell := range m.shells.Seq() {
		completedAt := shell.completedAt.Load()
		if completedAt > 0 && now-completedAt > retentionSeconds {
			toRemove = append(toRemove, shell.ID)
		}
	}

	for _, id := range toRemove {
		m.Remove(id)
	}

	return len(toRemove)
}

// KillAll terminates all background shells. The provided context bounds how
// long the function waits for each shell to exit.
func (m *BackgroundShellManager) KillAll(ctx context.Context) {
	shells := slices.Collect(m.shells.Seq())
	m.shells.Reset(map[string]*BackgroundShell{})

	var wg sync.WaitGroup
	for _, shell := range shells {
		wg.Go(func() {
			shell.cancel()
			select {
			case <-shell.done:
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
}

// GetOutput returns the current output of a background shell.
func (bs *BackgroundShell) GetOutput() (stdout string, stderr string, done bool, err error) {
	select {
	case <-bs.done:
		return bs.stdout.String(), bs.stderr.String(), true, bs.exitErr
	default:
		return bs.stdout.String(), bs.stderr.String(), false, nil
	}
}

// IsDone checks if the background shell has finished execution.
func (bs *BackgroundShell) IsDone() bool {
	select {
	case <-bs.done:
		return true
	default:
		return false
	}
}

// Wait blocks until the background shell completes.
func (bs *BackgroundShell) Wait() {
	<-bs.done
}

func (bs *BackgroundShell) WaitContext(ctx context.Context) bool {
	select {
	case <-bs.done:
		return true
	case <-ctx.Done():
		return false
	}
}
