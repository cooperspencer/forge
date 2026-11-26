package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// LogStore persists job logs as plain files and fans out appended chunks to
// live subscribers (TUI sessions, API streams).
type LogStore struct {
	dir  string
	mu   sync.Mutex
	subs map[int64]map[chan []byte]struct{}
}

func NewLogStore(dir string) (*LogStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LogStore{dir: dir, subs: map[int64]map[chan []byte]struct{}{}}, nil
}

func (l *LogStore) path(jobID int64) string {
	return filepath.Join(l.dir, fmt.Sprintf("%d.log", jobID))
}

func (l *LogStore) Append(jobID int64, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path(jobID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	f.Close()
	cp := append([]byte(nil), b...)
	for ch := range l.subs[jobID] {
		select {
		case ch <- cp:
		default: // slow consumer: drop; it can re-read the file
		}
	}
	return err
}

// Reset truncates a log (used when a job is retried).
func (l *LogStore) Reset(jobID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	os.Remove(l.path(jobID))
}

func (l *LogStore) Read(jobID int64) ([]byte, error) {
	b, err := os.ReadFile(l.path(jobID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

// Subscribe returns the current log content and a channel of subsequent
// chunks. Reading the snapshot and registering happen under one lock so no
// bytes are lost or duplicated. Call cancel when done.
func (l *LogStore) Subscribe(jobID int64) (snapshot []byte, ch <-chan []byte, cancel func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot, _ = os.ReadFile(l.path(jobID))
	c := make(chan []byte, 256)
	if l.subs[jobID] == nil {
		l.subs[jobID] = map[chan []byte]struct{}{}
	}
	l.subs[jobID][c] = struct{}{}
	var once sync.Once
	return snapshot, c, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.subs[jobID], c)
			if len(l.subs[jobID]) == 0 {
				delete(l.subs, jobID)
			}
			l.mu.Unlock()
		})
	}
}

// Broker is a tiny pub/sub for "something changed" notifications so UIs can
// refresh without polling the database aggressively.
type Broker struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func NewBroker() *Broker { return &Broker{subs: map[chan string]struct{}{}} }

func (b *Broker) Publish(topic string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- topic:
		default:
		}
	}
}

func (b *Broker) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}
