// Package cache stores exact and semantic cache entries [CCH-01, CCH-02, CCH-08]. The
// Redis backend is shared by every instance; the memory backend is a bounded LRU.
package cache

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

// Entry is one cached response.
type Entry struct {
	Key           string          `json:"key"`
	Namespace     string          `json:"ns"`
	Tenant        string          `json:"tenant"`
	Model         string          `json:"model"` // requested model
	Deployment    string          `json:"deployment"`
	Provider      string          `json:"provider"`
	Policy        string          `json:"policy"`
	PolicyVersion int             `json:"policy_version"`
	CostUSD       float64         `json:"cost_usd"` // what producing it cost
	InputTokens   int             `json:"input_tokens"`
	OutputTokens  int             `json:"output_tokens"`
	Response      json.RawMessage `json:"response"` // canonical ChatResponse
	Created       time.Time       `json:"created"`
}

// Candidate is a semantic-cache neighbour.
type Candidate struct {
	Key string    `json:"k"`
	Vec []float32 `json:"v"`
	At  int64     `json:"t"`
	// Salient fingerprints the prompt's numbers, quoted strings and proper nouns. A
	// semantic hit requires an exact match: "invoice INV-2041" and "invoice INV-7730"
	// embed almost identically but need different answers [CCH-09].
	Salient string `json:"s,omitempty"`
}

var (
	salientNum   = regexp.MustCompile(`\d+(?:[.,:/-]\d+)*`)
	salientQuote = regexp.MustCompile("\"[^\"]{1,80}\"|'[^'\\s][^']{0,79}'|`[^`]{1,80}`")
)

// properNouns returns capitalised words that do not start a sentence (sentence starts
// change freely between paraphrases).
func properNouns(text string) []string {
	var out []string
	sentenceStart := true
	for _, f := range strings.Fields(text) {
		w := strings.TrimFunc(f, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if w != "" {
			r, _ := utf8.DecodeRuneInString(w)
			if unicode.IsUpper(r) && !sentenceStart && len(w) > 1 {
				out = append(out, w)
			}
		}
		last, _ := utf8.DecodeLastRuneInString(f)
		sentenceStart = strings.ContainsRune(".!?:—-–", last) || f == "—" || f == "-"
	}
	return out
}

// Salient returns the fingerprint of the entities in text.
func Salient(text string) string {
	set := map[string]bool{}
	for _, m := range salientNum.FindAllString(text, -1) {
		set["n:"+m] = true
	}
	for _, m := range salientQuote.FindAllString(text, -1) {
		set["q:"+strings.ToLower(m)] = true
	}
	for _, w := range properNouns(text) {
		set["p:"+strings.ToLower(w)] = true
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|")
}

// Filter selects entries to invalidate; zero fields match all.
type Filter struct {
	Policy    string
	Model     string
	OlderThan time.Duration
}

func (f Filter) Match(e *Entry, now time.Time) bool {
	if f.Policy != "" && e.Policy != f.Policy {
		return false
	}
	if f.Model != "" && e.Model != f.Model && e.Deployment != f.Model {
		return false
	}
	if f.OlderThan > 0 && now.Sub(e.Created) < f.OlderThan {
		return false
	}
	return true
}

type Store interface {
	Get(ctx context.Context, ns, key string) (*Entry, error)
	Put(ctx context.Context, e *Entry, ttl time.Duration, maxEntries int) error
	Candidates(ctx context.Context, ns, bucket string) ([]Candidate, error)
	PutVector(ctx context.Context, ns, bucket string, c Candidate, maxPerBucket int, ttl time.Duration) error
	// Invalidate removes matching entries in namespace ns ("" = every namespace).
	Invalidate(ctx context.Context, ns string, f Filter) (int, error)
}

// Cosine similarity of two unit-normalised vectors.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

// ---------- memory ----------

type Memory struct {
	mu      sync.Mutex
	lru     *list.List
	entries map[string]*list.Element // ns|key -> element(*memEntry)
	vectors map[string]*memBucket    // ns|bucket
	// MaxBuckets bounds the number of semantic buckets (one per distinct conversation
	// prefix and output parameters); expired and then least recently written buckets go.
	MaxBuckets int
}

type memBucket struct {
	cands   []Candidate
	expires time.Time
	written time.Time
}

type memEntry struct {
	e       *Entry
	expires time.Time
}

func NewMemory() *Memory {
	return &Memory{lru: list.New(), entries: map[string]*list.Element{}, vectors: map[string]*memBucket{}, MaxBuckets: 20000}
}

func (m *Memory) Get(_ context.Context, ns, key string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	el, ok := m.entries[ns+"|"+key]
	if !ok {
		return nil, nil
	}
	me := el.Value.(*memEntry)
	if time.Now().After(me.expires) {
		m.lru.Remove(el)
		delete(m.entries, ns+"|"+key)
		return nil, nil
	}
	m.lru.MoveToFront(el)
	return me.e, nil
}

func (m *Memory) Put(_ context.Context, e *Entry, ttl time.Duration, maxEntries int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := e.Namespace + "|" + e.Key
	if el, ok := m.entries[k]; ok {
		el.Value = &memEntry{e: e, expires: time.Now().Add(ttl)}
		m.lru.MoveToFront(el)
	} else {
		m.entries[k] = m.lru.PushFront(&memEntry{e: e, expires: time.Now().Add(ttl)})
	}
	for maxEntries > 0 && m.lru.Len() > maxEntries {
		old := m.lru.Back()
		me := old.Value.(*memEntry)
		m.lru.Remove(old)
		delete(m.entries, me.e.Namespace+"|"+me.e.Key)
	}
	return nil
}

func (m *Memory) Candidates(_ context.Context, ns, bucket string) ([]Candidate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.vectors[ns+"|"+bucket]
	if b == nil {
		return nil, nil
	}
	if time.Now().After(b.expires) {
		delete(m.vectors, ns+"|"+bucket)
		return nil, nil
	}
	return append([]Candidate(nil), b.cands...), nil
}

func (m *Memory) PutVector(_ context.Context, ns, bucket string, c Candidate, maxPerBucket int, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	k := ns + "|" + bucket
	b := m.vectors[k]
	if b == nil || now.After(b.expires) {
		b = &memBucket{}
		m.vectors[k] = b
		m.pruneBuckets(now)
	}
	b.written, b.expires = now, now.Add(ttl)
	for i := range b.cands {
		if b.cands[i].Key == c.Key {
			b.cands[i] = c
			return nil
		}
	}
	b.cands = append(b.cands, c)
	if maxPerBucket > 0 && len(b.cands) > maxPerBucket {
		b.cands = b.cands[len(b.cands)-maxPerBucket:]
	}
	return nil
}

// pruneBuckets runs when a bucket is added beyond MaxBuckets: it drops expired buckets,
// then the least recently written tenth, so the sweep is amortised.
func (m *Memory) pruneBuckets(now time.Time) {
	if m.MaxBuckets <= 0 || len(m.vectors) <= m.MaxBuckets {
		return
	}
	for k, b := range m.vectors {
		if now.After(b.expires) {
			delete(m.vectors, k)
		}
	}
	if over := len(m.vectors) - m.MaxBuckets*9/10; over > 0 {
		type kv struct {
			k string
			t time.Time
		}
		all := make([]kv, 0, len(m.vectors))
		for k, b := range m.vectors {
			all = append(all, kv{k, b.written})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
		for _, x := range all[:over] {
			delete(m.vectors, x.k)
		}
	}
}

func (m *Memory) Invalidate(_ context.Context, ns string, f Filter) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	n := 0
	for k, el := range m.entries {
		me := el.Value.(*memEntry)
		if (ns == "" || me.e.Namespace == ns) && f.Match(me.e, now) {
			m.lru.Remove(el)
			delete(m.entries, k)
			n++
		}
	}
	return n, nil
}

// ---------- Redis ----------

type Redis struct {
	c      redis.UniversalClient
	prefix string
}

func NewRedis(c redis.UniversalClient, prefix string) *Redis { return &Redis{c: c, prefix: prefix} }

func (r *Redis) ek(ns, key string) string { return fmt.Sprintf("%sce:%s:%s", r.prefix, ns, key) }
func (r *Redis) ik(ns string) string      { return fmt.Sprintf("%sci:%s", r.prefix, ns) }
func (r *Redis) vk(ns, b string) string   { return fmt.Sprintf("%scv:%s:%s", r.prefix, ns, b) }
func (r *Redis) nsSet() string            { return r.prefix + "cns" }

func (r *Redis) Get(ctx context.Context, ns, key string) (*Entry, error) {
	b, err := r.c.Get(ctx, r.ek(ns, key)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, nil
	}
	return &e, nil
}

func (r *Redis) Put(ctx context.Context, e *Entry, ttl time.Duration, maxEntries int) error {
	b, _ := json.Marshal(e)
	pipe := r.c.TxPipeline()
	pipe.Set(ctx, r.ek(e.Namespace, e.Key), b, ttl)
	pipe.ZAdd(ctx, r.ik(e.Namespace), redis.Z{Score: float64(e.Created.Unix()), Member: e.Key})
	pipe.SAdd(ctx, r.nsSet(), e.Namespace)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	// Bound the namespace: drop the oldest entries beyond maxEntries [CCH-08].
	if maxEntries > 0 {
		n, err := r.c.ZCard(ctx, r.ik(e.Namespace)).Result()
		if err == nil && n > int64(maxEntries) {
			old, _ := r.c.ZRange(ctx, r.ik(e.Namespace), 0, n-int64(maxEntries)-1).Result()
			for _, k := range old {
				r.c.Del(ctx, r.ek(e.Namespace, k))
				r.c.ZRem(ctx, r.ik(e.Namespace), k)
			}
		}
	}
	return nil
}

func (r *Redis) Candidates(ctx context.Context, ns, bucket string) ([]Candidate, error) {
	vals, err := r.c.HGetAll(ctx, r.vk(ns, bucket)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(vals))
	for _, v := range vals {
		var c Candidate
		if json.Unmarshal([]byte(v), &c) == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *Redis) PutVector(ctx context.Context, ns, bucket string, c Candidate, maxPerBucket int, ttl time.Duration) error {
	b, _ := json.Marshal(c)
	k := r.vk(ns, bucket)
	if err := r.c.HSet(ctx, k, c.Key, b).Err(); err != nil {
		return err
	}
	r.c.Expire(ctx, k, ttl)
	if maxPerBucket > 0 {
		if n, _ := r.c.HLen(ctx, k).Result(); n > int64(maxPerBucket) {
			cands, _ := r.Candidates(ctx, ns, bucket)
			oldest := cands[0]
			for _, x := range cands {
				if x.At < oldest.At {
					oldest = x
				}
			}
			r.c.HDel(ctx, k, oldest.Key)
		}
	}
	return nil
}

func (r *Redis) Invalidate(ctx context.Context, ns string, f Filter) (int, error) {
	namespaces := []string{ns}
	if ns == "" {
		var err error
		if namespaces, err = r.c.SMembers(ctx, r.nsSet()).Result(); err != nil {
			return 0, err
		}
	}
	now := time.Now()
	n := 0
	for _, ns := range namespaces {
		keys, err := r.c.ZRange(ctx, r.ik(ns), 0, -1).Result()
		if err != nil {
			return n, err
		}
		for _, k := range keys {
			e, err := r.Get(ctx, ns, k)
			if err != nil {
				return n, err
			}
			if e == nil {
				r.c.ZRem(ctx, r.ik(ns), k)
				continue
			}
			if f.Match(e, now) {
				r.c.Del(ctx, r.ek(ns, k))
				r.c.ZRem(ctx, r.ik(ns), k)
				n++
			}
		}
	}
	return n, nil
}
