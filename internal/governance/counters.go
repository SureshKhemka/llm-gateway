package governance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Counters is the shared hot state behind budgets and rate limits. The Redis
// implementation makes limits hold across every gateway instance [BUD-05]; the memory
// implementation serves single-instance development.
type Counters interface {
	// Reserve atomically adds amount to the reserved total of every key, unless any key's
	// spent+reserved+amount would exceed its limit; it returns the index of the first key
	// that would overflow, or -1 [BUD-03].
	Reserve(ctx context.Context, keys []string, limits []int64, amount int64, ttl time.Duration) (int, error)
	// Settle replaces a reservation with the actual amount on every key.
	Settle(ctx context.Context, keys []string, reserved, actual int64, ttl time.Duration) error
	// Usage returns spent and reserved for each key.
	Usage(ctx context.Context, keys []string) (spent, reserved []int64, err error)
	// Window checks and, if every limit holds, increments sliding one-minute counters.
	// It returns the first failing index (-1 if admitted) and the current counts.
	Window(ctx context.Context, keys []string, limits, incr []int64, now time.Time) (int, []int64, error)
	// Add increments sliding-window counters without checking (post-hoc token debits).
	Add(ctx context.Context, keys []string, incr []int64, now time.Time) error
	// Acquire adds id to a concurrency set unless it holds limit live members.
	Acquire(ctx context.Context, key, id string, limit int64, ttl time.Duration) (bool, error)
	Release(ctx context.Context, key, id string) error
	// Latch returns true the first time key is latched within ttl (alert dedup) [BUD-07].
	Latch(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// Publish/Subscribe broadcast revocations and reload signals across instances.
	Publish(ctx context.Context, channel, msg string) error
	Subscribe(ctx context.Context, channel string, fn func(msg string))
	Ping(ctx context.Context) error
}

// ErrUnavailable wraps backend failures; budgets fail closed on it by default [NFR-08].
var ErrUnavailable = errors.New("counters unavailable")

// ---------- Redis ----------

type RedisCounters struct {
	c      redis.UniversalClient
	prefix string
}

func NewRedisCounters(c redis.UniversalClient, prefix string) *RedisCounters {
	return &RedisCounters{c: c, prefix: prefix}
}

func (r *RedisCounters) k(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = r.prefix + k
	}
	return out
}

func wrap(err error) error {
	if err == nil || errors.Is(err, redis.Nil) {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

var reserveScript = redis.NewScript(`
local amount = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
for i, k in ipairs(KEYS) do
  local limit = tonumber(ARGV[2 + i])
  local s = tonumber(redis.call('HGET', k, 's') or '0')
  local r = tonumber(redis.call('HGET', k, 'r') or '0')
  if s + r + amount > limit then return i end
end
for i, k in ipairs(KEYS) do
  redis.call('HINCRBY', k, 'r', amount)
  redis.call('EXPIRE', k, ttl)
end
return 0`)

func (r *RedisCounters) Reserve(ctx context.Context, keys []string, limits []int64, amount int64, ttl time.Duration) (int, error) {
	args := []any{amount, int64(ttl.Seconds()) + 1}
	for _, l := range limits {
		args = append(args, l)
	}
	n, err := reserveScript.Run(ctx, r.c, r.k(keys), args...).Int()
	if err != nil {
		return -1, wrap(err)
	}
	return n - 1, nil
}

var settleScript = redis.NewScript(`
local reserved = tonumber(ARGV[1])
local actual = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
for _, k in ipairs(KEYS) do
  redis.call('HINCRBY', k, 'r', -reserved)
  redis.call('HINCRBY', k, 's', actual)
  redis.call('EXPIRE', k, ttl)
end
return 0`)

func (r *RedisCounters) Settle(ctx context.Context, keys []string, reserved, actual int64, ttl time.Duration) error {
	return wrap(settleScript.Run(ctx, r.c, r.k(keys), reserved, actual, int64(ttl.Seconds())+1).Err())
}

func (r *RedisCounters) Usage(ctx context.Context, keys []string) ([]int64, []int64, error) {
	pipe := r.c.Pipeline()
	cmds := make([]*redis.SliceCmd, len(keys))
	for i, k := range r.k(keys) {
		cmds[i] = pipe.HMGet(ctx, k, "s", "r")
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, nil, wrap(err)
	}
	spent, res := make([]int64, len(keys)), make([]int64, len(keys))
	for i, c := range cmds {
		v := c.Val()
		spent[i], res[i] = toInt(v[0]), toInt(v[1])
	}
	return spent, res, nil
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case int64:
		return x
	}
	return 0
}

// Sliding window over two fixed one-minute buckets: count = cur + prev × (1 − elapsed).
var windowScript = redis.NewScript(`
local n = #KEYS / 2
local frac = tonumber(ARGV[1])
local check = tonumber(ARGV[2])
local counts = {}
for i = 1, n do
  local cur = tonumber(redis.call('GET', KEYS[2*i-1]) or '0')
  local prev = tonumber(redis.call('GET', KEYS[2*i]) or '0')
  counts[i] = math.floor(cur + prev * (1 - frac))
end
if check == 1 then
  for i = 1, n do
    local limit = tonumber(ARGV[2 + 2*i - 1])
    local inc = tonumber(ARGV[2 + 2*i])
    if limit > 0 and counts[i] + inc > limit then
      table.insert(counts, 1, i)
      return counts
    end
  end
end
for i = 1, n do
  local inc = tonumber(ARGV[2 + 2*i])
  if inc ~= 0 then
    redis.call('INCRBY', KEYS[2*i-1], inc)
    redis.call('EXPIRE', KEYS[2*i-1], 130)
  end
  counts[i] = counts[i] + inc
end
table.insert(counts, 1, 0)
return counts`)

func windowKeys(prefix string, keys []string, now time.Time) []string {
	min := now.Unix() / 60
	out := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s%s:%d", prefix, k, min), fmt.Sprintf("%s%s:%d", prefix, k, min-1))
	}
	return out
}

func (r *RedisCounters) window(ctx context.Context, keys []string, limits, incr []int64, now time.Time, check bool) (int, []int64, error) {
	frac := float64(now.Unix()%60) / 60
	chk := 0
	if check {
		chk = 1
	}
	args := []any{frac, chk}
	for i := range keys {
		var l int64
		if limits != nil {
			l = limits[i]
		}
		args = append(args, l, incr[i])
	}
	vals, err := windowScript.Run(ctx, r.c, windowKeys(r.prefix, keys, now), args...).Int64Slice()
	if err != nil {
		return -1, nil, wrap(err)
	}
	return int(vals[0]) - 1, vals[1:], nil
}

func (r *RedisCounters) Window(ctx context.Context, keys []string, limits, incr []int64, now time.Time) (int, []int64, error) {
	return r.window(ctx, keys, limits, incr, now, true)
}

func (r *RedisCounters) Add(ctx context.Context, keys []string, incr []int64, now time.Time) error {
	_, _, err := r.window(ctx, keys, nil, incr, now, false)
	return err
}

var acquireScript = redis.NewScript(`
local now = tonumber(ARGV[1])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
redis.call('ZADD', KEYS[1], now + tonumber(ARGV[3]), ARGV[4])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]) + 1)
return 1`)

func (r *RedisCounters) Acquire(ctx context.Context, key, id string, limit int64, ttl time.Duration) (bool, error) {
	n, err := acquireScript.Run(ctx, r.c, []string{r.prefix + key}, time.Now().Unix(), limit, int64(ttl.Seconds())+1, id).Int()
	if err != nil {
		return false, wrap(err)
	}
	return n == 1, nil
}

func (r *RedisCounters) Release(ctx context.Context, key, id string) error {
	return wrap(r.c.ZRem(ctx, r.prefix+key, id).Err())
}

func (r *RedisCounters) Latch(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := r.c.SetNX(ctx, r.prefix+key, "1", ttl).Result()
	return ok, wrap(err)
}

func (r *RedisCounters) Publish(ctx context.Context, channel, msg string) error {
	return wrap(r.c.Publish(ctx, r.prefix+channel, msg).Err())
}

func (r *RedisCounters) Subscribe(ctx context.Context, channel string, fn func(string)) {
	sub := r.c.Subscribe(ctx, r.prefix+channel)
	go func() {
		defer sub.Close()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				fn(m.Payload)
			}
		}
	}()
}

func (r *RedisCounters) Ping(ctx context.Context) error { return wrap(r.c.Ping(ctx).Err()) }

// ---------- memory ----------

// MemCounters implements Counters in-process (one gateway instance).
type MemCounters struct {
	mu      sync.Mutex
	budgets map[string]*[2]int64 // spent, reserved
	windows map[string]int64
	sets    map[string]map[string]time.Time
	latches map[string]time.Time
	subs    map[string][]func(string)
}

func NewMemCounters() *MemCounters {
	return &MemCounters{budgets: map[string]*[2]int64{}, windows: map[string]int64{}, sets: map[string]map[string]time.Time{},
		latches: map[string]time.Time{}, subs: map[string][]func(string){}}
}

func (m *MemCounters) b(k string) *[2]int64 {
	v, ok := m.budgets[k]
	if !ok {
		v = &[2]int64{}
		m.budgets[k] = v
	}
	return v
}

func (m *MemCounters) Reserve(_ context.Context, keys []string, limits []int64, amount int64, _ time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, k := range keys {
		v := m.b(k)
		if v[0]+v[1]+amount > limits[i] {
			return i, nil
		}
	}
	for _, k := range keys {
		m.b(k)[1] += amount
	}
	return -1, nil
}

func (m *MemCounters) Settle(_ context.Context, keys []string, reserved, actual int64, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		v := m.b(k)
		v[1] -= reserved
		v[0] += actual
	}
	return nil
}

func (m *MemCounters) Usage(_ context.Context, keys []string) ([]int64, []int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, r := make([]int64, len(keys)), make([]int64, len(keys))
	for i, k := range keys {
		v := m.b(k)
		s[i], r[i] = v[0], v[1]
	}
	return s, r, nil
}

func (m *MemCounters) window(keys []string, limits, incr []int64, now time.Time, check bool) (int, []int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	frac := float64(now.Unix()%60) / 60
	wk := windowKeys("", keys, now)
	counts := make([]int64, len(keys))
	for i := range keys {
		counts[i] = int64(float64(m.windows[wk[2*i]]) + float64(m.windows[wk[2*i+1]])*(1-frac))
	}
	if check {
		for i := range keys {
			if limits[i] > 0 && counts[i]+incr[i] > limits[i] {
				return i, counts
			}
		}
	}
	for i := range keys {
		m.windows[wk[2*i]] += incr[i]
		counts[i] += incr[i]
	}
	return -1, counts
}

func (m *MemCounters) Window(_ context.Context, keys []string, limits, incr []int64, now time.Time) (int, []int64, error) {
	i, c := m.window(keys, limits, incr, now, true)
	return i, c, nil
}

func (m *MemCounters) Add(_ context.Context, keys []string, incr []int64, now time.Time) error {
	m.window(keys, nil, incr, now, false)
	return nil
}

func (m *MemCounters) Acquire(_ context.Context, key, id string, limit int64, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.sets[key]
	if !ok {
		set = map[string]time.Time{}
		m.sets[key] = set
	}
	now := time.Now()
	for k, exp := range set {
		if now.After(exp) {
			delete(set, k)
		}
	}
	if int64(len(set)) >= limit {
		return false, nil
	}
	set[id] = now.Add(ttl)
	return true, nil
}

func (m *MemCounters) Release(_ context.Context, key, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sets[key], id)
	return nil
}

func (m *MemCounters) Latch(_ context.Context, key string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if exp, ok := m.latches[key]; ok && time.Now().Before(exp) {
		return false, nil
	}
	m.latches[key] = time.Now().Add(ttl)
	return true, nil
}

func (m *MemCounters) Publish(_ context.Context, channel, msg string) error {
	m.mu.Lock()
	subs := append([]func(string){}, m.subs[channel]...)
	m.mu.Unlock()
	for _, fn := range subs {
		fn(msg)
	}
	return nil
}

func (m *MemCounters) Subscribe(_ context.Context, channel string, fn func(string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[channel] = append(m.subs[channel], fn)
}

func (m *MemCounters) Ping(context.Context) error { return nil }
