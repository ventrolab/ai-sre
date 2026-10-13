package guard

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeKV is an in-memory kvStore with revisions; conflicts > 0 makes the next writes fail
// as if another replica had written first.
type fakeKV struct {
	mu        sync.Mutex
	data      map[string][]byte
	rev       map[string]uint64
	seq       uint64
	conflicts int
	down      bool
}

func newFakeKV() *fakeKV { return &fakeKV{data: map[string][]byte{}, rev: map[string]uint64{}} }

func (f *fakeKV) Get(key string) ([]byte, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, 0, errors.New("nats: timeout")
	}
	v, ok := f.data[key]
	if !ok {
		return nil, 0, errKeyNotFound
	}
	return v, f.rev[key], nil
}

func (f *fakeKV) put(key string, value []byte, want uint64) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return 0, errors.New("nats: timeout")
	}
	if f.conflicts > 0 {
		f.conflicts--
		f.seq++
		f.rev[key] = f.seq
		return 0, errors.New("wrong last sequence")
	}
	if f.rev[key] != want {
		return 0, errors.New("wrong last sequence")
	}
	f.seq++
	f.data[key], f.rev[key] = value, f.seq
	return f.seq, nil
}

func (f *fakeKV) Create(key string, value []byte) (uint64, error) { return f.put(key, value, 0) }
func (f *fakeKV) Update(key string, value []byte, rev uint64) (uint64, error) {
	return f.put(key, value, rev)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func kvLimiterAt(kv kvStore, now *time.Time) *KVLimiter {
	l := NewKVLimiter(func() (kvStore, error) { return kv, nil }, quiet)
	l.now = func() time.Time { return *now }
	l.mem.now = l.now
	return l
}

func TestKVLimiterSurvivesRestartAndExpires(t *testing.T) {
	kv, now := newFakeKV(), time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	a := kvLimiterAt(kv, &now)
	a.Record("c/ns/web///")
	a.Record("c/ns/web///")
	a.Record("c/ns/other///")

	b := kvLimiterAt(kv, &now) // a restarted or second replica: empty memory
	if n := b.Count("c/ns/web///"); n != 2 {
		t.Fatalf("count after restart = %d, want 2", n)
	}
	now = now.Add(30 * time.Minute)
	b.Record("c/ns/web///")
	now = now.Add(31 * time.Minute) // the first two are older than an hour now
	if n := b.Count("c/ns/web///"); n != 1 {
		t.Fatalf("count after an hour = %d, want 1", n)
	}
}

func TestKVLimiterRetriesOnConflict(t *testing.T) {
	kv, now := newFakeKV(), time.Now()
	l := kvLimiterAt(kv, &now)
	l.Record("k")
	kv.conflicts = 3
	l.Record("k")
	if n := kvLimiterAt(kv, &now).Count("k"); n != 2 {
		t.Fatalf("count = %d, want 2 after conflicting writes", n)
	}
}

func TestKVLimiterFallsBackToMemory(t *testing.T) {
	kv, now := newFakeKV(), time.Now()
	l := kvLimiterAt(kv, &now)
	l.Record("k")
	kv.down = true
	l.Record("k") // counted in memory only
	if n := l.Count("k"); n != 2 {
		t.Fatalf("count with store down = %d, want 2 from memory", n)
	}
	kv.down = false
	if n := l.Count("k"); n != 2 {
		t.Fatalf("count after recovery = %d, want max(kv 1, memory 2)", n)
	}
}

func TestKVLimiterOpensLazily(t *testing.T) {
	kv, now, opens := newFakeKV(), time.Now(), 0
	l := NewKVLimiter(func() (kvStore, error) {
		opens++
		if opens == 1 {
			return nil, errors.New("jetstream not ready")
		}
		return kv, nil
	}, quiet)
	l.now = func() time.Time { return now }
	l.mem.now = l.now
	l.Record("k") // open fails, memory only
	if n := l.Count("k"); n != 1 || opens != 1 {
		t.Fatalf("count = %d, opens = %d; want 1, 1 (no retry within 30 s)", n, opens)
	}
	now = now.Add(31 * time.Second)
	l.Record("k")
	if opens != 2 || kvLimiterAt(kv, &now).Count("k") != 1 {
		t.Fatalf("bucket not used after a successful open (opens = %d)", opens)
	}
}

func TestKVLimiterIgnoresBrokenEntry(t *testing.T) {
	kv, now := newFakeKV(), time.Now()
	kv.data[kvKey("k")], kv.rev[kvKey("k")], kv.seq = []byte("not json"), 1, 1
	l := kvLimiterAt(kv, &now)
	if n := l.Count("k"); n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
	l.Record("k")
	if n := kvLimiterAt(kv, &now).Count("k"); n != 1 {
		t.Fatalf("broken entry not replaced, count = %d", n)
	}
}
