package store

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Writer batches cost and decision records off the request path. If the store is
// unavailable, batches spill to an append-only local WAL file and are replayed when the
// store recovers [NFR-07, NFR-08].
type Writer struct {
	st       Store
	costs    chan CostRecord
	decs     chan DecisionRecord
	interval time.Duration
	walPath  string
	log      *slog.Logger

	mu      sync.Mutex
	pending sync.WaitGroup
	stop    chan struct{}
	done    chan struct{}
}

func NewWriter(st Store, walDir string, interval time.Duration, log *slog.Logger) *Writer {
	if interval == 0 {
		interval = time.Second
	}
	w := &Writer{st: st, costs: make(chan CostRecord, 65536), decs: make(chan DecisionRecord, 65536),
		interval: interval, log: log, stop: make(chan struct{}), done: make(chan struct{})}
	if walDir != "" {
		_ = os.MkdirAll(walDir, 0o700)
		w.walPath = filepath.Join(walDir, "records.wal")
	}
	go w.loop()
	return w
}

func (w *Writer) Cost(r CostRecord) {
	w.pending.Add(1)
	select {
	case w.costs <- r:
	default:
		// Queue full: write synchronously rather than drop the record.
		w.flushCosts([]CostRecord{r})
		w.pending.Done()
	}
}

func (w *Writer) Decision(d DecisionRecord) {
	w.pending.Add(1)
	select {
	case w.decs <- d:
	default:
		_ = w.st.PutDecision(context.Background(), d)
		w.pending.Done()
	}
}

// Flush blocks until every record enqueued so far has been written.
func (w *Writer) Flush() { w.pending.Wait() }

func (w *Writer) Close() {
	close(w.stop)
	<-w.done
}

func (w *Writer) loop() {
	defer close(w.done)
	t := time.NewTicker(w.interval)
	defer t.Stop()
	var costs []CostRecord
	var decs []DecisionRecord
	flush := func() {
		if len(costs) > 0 {
			w.flushCosts(costs)
			for range costs {
				w.pending.Done()
			}
			costs = nil
		}
		for _, d := range decs {
			if err := w.st.PutDecision(context.Background(), d); err != nil && w.log != nil {
				w.log.Warn("decision record write failed", "err", err)
			}
			w.pending.Done()
		}
		decs = nil
		w.replayWAL()
	}
	for {
		select {
		case r := <-w.costs:
			costs = append(costs, r)
			if len(costs) >= 500 {
				flush()
			}
		case d := <-w.decs:
			decs = append(decs, d)
			if len(decs) >= 500 {
				flush()
			}
		case <-t.C:
			flush()
		case <-w.stop:
			for {
				select {
				case r := <-w.costs:
					costs = append(costs, r)
				case d := <-w.decs:
					decs = append(decs, d)
				default:
					flush()
					return
				}
			}
		}
		// Drain quickly when idle so Flush() returns promptly in tests and at low load.
		if len(w.costs) == 0 && len(w.decs) == 0 && (len(costs) > 0 || len(decs) > 0) {
			flush()
		}
	}
}

func (w *Writer) flushCosts(recs []CostRecord) {
	if err := w.st.InsertCostRecords(context.Background(), recs); err != nil {
		if w.log != nil {
			w.log.Warn("cost record write failed; spilling to WAL", "err", err, "n", len(recs))
		}
		w.spill(recs)
	}
}

func (w *Writer) spill(recs []CostRecord) {
	if w.walPath == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.walPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range recs {
		_ = enc.Encode(r)
	}
	_ = f.Sync()
}

func (w *Writer) replayWAL() {
	if w.walPath == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.Open(w.walPath)
	if err != nil {
		return
	}
	var recs []CostRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r CostRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			recs = append(recs, r)
		}
	}
	f.Close()
	if len(recs) == 0 {
		_ = os.Remove(w.walPath)
		return
	}
	// Inserts are idempotent by request ID, so a partial replay is safe to repeat.
	if err := w.st.InsertCostRecords(context.Background(), recs); err == nil {
		_ = os.Remove(w.walPath)
	}
}
