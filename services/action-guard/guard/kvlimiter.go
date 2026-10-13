package guard

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RateLimitBucket keeps executed actions per target; entries expire after an hour.
const RateLimitBucket = "aisre_guard_actions"

var limiterErrors = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "aisre_guard_rate_limit_store_errors_total",
	Help: "Rate limit store failures; the guard then uses its in-memory counts",
}, []string{"op"})

var errKeyNotFound = errors.New("key not found")

// kvStore is the part of a NATS KV bucket the limiter needs.
type kvStore interface {
	Get(key string) (value []byte, revision uint64, err error)
	Create(key string, value []byte) (uint64, error)
	Update(key string, value []byte, revision uint64) (uint64, error)
}

// KVLimiter persists action timestamps in NATS KV, so limits survive restarts and are shared
// between replicas. One key per target holds a JSON list of unix-nano timestamps, written with
// compare-and-set. Every action is also recorded in memory; while the bucket is unavailable the
// in-memory count is used, and the larger of both counts always wins.
type KVLimiter struct {
	open func() (kvStore, error)
	mem  *RateLimiter
	now  func() time.Time
	log  *slog.Logger

	mu       sync.Mutex
	kv       kvStore
	nextOpen time.Time
}

func NewKVLimiter(open func() (kvStore, error), log *slog.Logger) *KVLimiter {
	return &KVLimiter{open: open, mem: NewRateLimiter(), now: time.Now, log: log}
}

// store returns the bucket, opening it on first use and at most every 30 s after a failure.
func (l *KVLimiter) store() (kvStore, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.kv != nil {
		return l.kv, nil
	}
	if l.now().Before(l.nextOpen) {
		return nil, errors.New("rate limit bucket not available yet")
	}
	kv, err := l.open()
	if err != nil {
		l.nextOpen = l.now().Add(30 * time.Second)
		return nil, fmt.Errorf("open bucket %s: %w", RateLimitBucket, err)
	}
	l.log.Info("rate limits are kept in NATS KV", "bucket", RateLimitBucket)
	l.kv = kv
	return kv, nil
}

func kvKey(target string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(target))
}

// load returns the timestamps of the last hour and the entry revision (0 if absent).
func (l *KVLimiter) load(key string) ([]int64, uint64, error) {
	kv, err := l.store()
	if err != nil {
		return nil, 0, err
	}
	raw, rev, err := kv.Get(kvKey(key))
	if errors.Is(err, errKeyNotFound) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	var all []int64
	if err := json.Unmarshal(raw, &all); err != nil {
		// A broken entry must not block the guard; it is overwritten on the next Record.
		l.log.Warn("rate limit entry unreadable, ignoring it", "target", key, "err", err)
		return nil, rev, nil
	}
	cutoff := l.now().Add(-time.Hour).UnixNano()
	kept := all[:0]
	for _, t := range all {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	return kept, rev, nil
}

func (l *KVLimiter) Count(key string) int {
	mem := l.mem.Count(key)
	ts, _, err := l.load(key)
	if err != nil {
		limiterErrors.WithLabelValues("count").Inc()
		l.log.Error("rate limit store read failed, using in-memory count", "target", key, "err", err)
		return mem
	}
	return max(len(ts), mem)
}

func (l *KVLimiter) Record(key string) {
	l.mem.Record(key)
	if err := l.record(key); err != nil {
		limiterErrors.WithLabelValues("record").Inc()
		l.log.Error("rate limit store write failed, action counted in memory only", "target", key, "err", err)
	}
}

func (l *KVLimiter) record(key string) error {
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var ts []int64
		var rev uint64
		if ts, rev, err = l.load(key); err != nil {
			return err
		}
		value, _ := json.Marshal(append(ts, l.now().UnixNano()))
		kv, _ := l.store() // load succeeded, so the bucket is open
		if rev == 0 {
			_, err = kv.Create(kvKey(key), value)
		} else {
			_, err = kv.Update(kvKey(key), value, rev)
		}
		if err == nil {
			return nil
		}
		// Another replica wrote in between: reload and retry.
	}
	return fmt.Errorf("after retries: %w", err)
}

// natsKV adapts a JetStream KV bucket to kvStore.
type natsKV struct{ kv nats.KeyValue }

func (n natsKV) Get(key string) ([]byte, uint64, error) {
	e, err := n.kv.Get(key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return nil, 0, errKeyNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return e.Value(), e.Revision(), nil
}

func (n natsKV) Create(key string, value []byte) (uint64, error) { return n.kv.Create(key, value) }

func (n natsKV) Update(key string, value []byte, rev uint64) (uint64, error) {
	return n.kv.Update(key, value, rev)
}

// openRateLimitBucket binds to the bucket, creating it on first start.
func openRateLimitBucket(nc *nats.Conn) (kvStore, error) {
	js, err := nc.JetStream(nats.MaxWait(3 * time.Second))
	if err != nil {
		return nil, err
	}
	kv, err := js.KeyValue(RateLimitBucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{
			Bucket:      RateLimitBucket,
			Description: "AI SRE Action Guard: executed actions per target",
			History:     1,
			TTL:         time.Hour,
			Storage:     nats.FileStorage,
		})
	}
	if err != nil {
		return nil, err
	}
	return natsKV{kv}, nil
}
