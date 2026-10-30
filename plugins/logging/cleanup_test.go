package logging

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/capsohq/bifrost/core/schemas"
	"github.com/capsohq/bifrost/framework/logstore"
)

// recordingStore wraps a LogStore and records every log ID that reaches
// BatchCreateIfNotExists. Optional delay simulates a slow underlying store
// so we can force entries to back up between the writeQueue channel buffer
// and batchWriter's local batch slice at Cleanup time.
type recordingStore struct {
	logstore.LogStore
	mu       sync.Mutex
	received []string
	delay    time.Duration
}

func (s *recordingStore) BatchCreateIfNotExists(ctx context.Context, entries []*logstore.Log) error {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	if err := s.LogStore.BatchCreateIfNotExists(ctx, entries); err != nil {
		return err
	}
	s.mu.Lock()
	for _, e := range entries {
		s.received = append(s.received, e.ID)
	}
	s.mu.Unlock()
	return nil
}

func (s *recordingStore) uniqueIDs() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct{}, len(s.received))
	for _, id := range s.received {
		out[id] = struct{}{}
	}
	return out
}

func makeTestLog(id string) *logstore.Log {
	now := time.Now()
	return &logstore.Log{
		ID:        id,
		Timestamp: now,
		CreatedAt: now,
		Provider:  "test-provider",
		Model:     "test-model",
		Status:    "success",
	}
}

// TestCleanupDrainsRecoveredBatchNoDrops covers the recovered-batch handoff:
// entries sit in batchWriter's in-memory batch (well under maxBatchSize, so
// no automatic flush has happened) when Cleanup runs. drainPending must
// recover and persist all of them.
func TestCleanupDrainsRecoveredBatchNoDrops(t *testing.T) {
	rec := &recordingStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, rec, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	const N = 500 // well under maxBatchSize (1000); batchWriter will not auto-flush
	for i := 0; i < N; i++ {
		plugin.enqueueLogEntry(makeTestLog(fmt.Sprintf("recovered-%d", i)), nil)
	}

	// Let batchWriter dequeue everything into its local batch. 100ms is far
	// less than batchInterval (2s), so no automatic flush should have run.
	time.Sleep(100 * time.Millisecond)

	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	got := rec.uniqueIDs()
	if len(got) != N {
		t.Fatalf("expected %d unique entries persisted via recovered batch, got %d", N, len(got))
	}
	for i := 0; i < N; i++ {
		id := fmt.Sprintf("recovered-%d", i)
		if _, ok := got[id]; !ok {
			t.Fatalf("entry %s was dropped during Cleanup", id)
		}
	}
	if dropped := plugin.droppedRequests.Load(); dropped != 0 {
		t.Fatalf("expected 0 dropped requests, got %d", dropped)
	}
}

// TestCleanupDrainsCombinedQueueAndBatchNoDrops covers the mixed path: a
// large burst combined with a slow underlying store guarantees that some
// entries land in writeQueue's channel buffer while others sit in
// batchWriter's local batch when Cleanup arrives. Both paths must be drained.
func TestCleanupDrainsCombinedQueueAndBatchNoDrops(t *testing.T) {
	rec := &recordingStore{
		LogStore: newTestStore(t),
		delay:    25 * time.Millisecond, // slow store keeps batchWriter busy so the channel buffer fills
	}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, rec, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	const N = 2500 // > maxBatchSize so batchWriter triggers at least two intermediate flushes
	for i := 0; i < N; i++ {
		plugin.enqueueLogEntry(makeTestLog(fmt.Sprintf("combined-%d", i)), nil)
	}

	// Call Cleanup while batchWriter is likely mid-processBatch (the 25ms
	// per-batch delay means the queue is still partially full). drainPending
	// must wait for the ownership handoff, then drain the remainder.
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	got := rec.uniqueIDs()
	if len(got) != N {
		t.Fatalf("expected %d unique entries persisted (mix of batchWriter + drainPending), got %d; dropped=%d",
			N, len(got), plugin.droppedRequests.Load())
	}
	for i := 0; i < N; i++ {
		id := fmt.Sprintf("combined-%d", i)
		if _, ok := got[id]; !ok {
			t.Fatalf("entry %s was dropped during Cleanup", id)
		}
	}
}

// TestCleanupRejectsNewSendsAfterClosed verifies the producer-side guard
// (p.closed.Load() in enqueueLogEntry) takes effect once Cleanup has run:
// subsequent enqueues are dropped at the source and do not panic.
func TestCleanupRejectsNewSendsAfterClosed(t *testing.T) {
	rec := &recordingStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, rec, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	plugin.enqueueLogEntry(makeTestLog("pre-cleanup"), nil)

	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	// After Cleanup, the channel is closed and p.closed is true. Producers
	// must short-circuit via the closed check (no panic, no enqueue).
	dropsBefore := plugin.droppedRequests.Load()
	plugin.enqueueLogEntry(makeTestLog("post-cleanup"), nil)
	dropsAfter := plugin.droppedRequests.Load()

	// The closed-check path returns silently without incrementing
	// droppedRequests (matches existing semantics at writer.go:251-253).
	if dropsBefore != dropsAfter {
		t.Fatalf("post-cleanup enqueue should be a silent no-op; droppedRequests went from %d to %d",
			dropsBefore, dropsAfter)
	}

	got := rec.uniqueIDs()
	if _, ok := got["pre-cleanup"]; !ok {
		t.Fatalf("pre-cleanup entry was dropped during Cleanup")
	}
	if _, ok := got["post-cleanup"]; ok {
		t.Fatalf("post-cleanup entry should never have reached the store")
	}
}

// TestCleanupIsIdempotent verifies the cleanupOnce guard: a second Cleanup
// call must be a no-op rather than re-cancelling, re-closing channels, or
// panicking.
func TestCleanupIsIdempotent(t *testing.T) {
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, newTestStore(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("first Cleanup() error = %v", err)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("second Cleanup() error = %v", err)
	}
}

func TestWriterConfigDefaultsAndInitOverrides(t *testing.T) {
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, newTestStore(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if plugin.writerConfig.MaxBatchSize != logstore.DefaultWriterMaxBatchSize {
		t.Fatalf("MaxBatchSize = %d, want %d", plugin.writerConfig.MaxBatchSize, logstore.DefaultWriterMaxBatchSize)
	}
	if cap(plugin.writeQueue) != logstore.DefaultWriterQueueCapacity {
		t.Fatalf("writeQueue cap = %d, want %d", cap(plugin.writeQueue), logstore.DefaultWriterQueueCapacity)
	}
	if cap(plugin.deferredUsageSem) != logstore.DefaultWriterDeferredUsageConcurrency {
		t.Fatalf("deferredUsageSem cap = %d, want %d", cap(plugin.deferredUsageSem), logstore.DefaultWriterDeferredUsageConcurrency)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	writer := &logstore.WriterConfig{
		MaxBatchSize:             7,
		BatchInterval:            "10ms",
		MaxBatchBytes:            1024,
		WriteQueueCapacity:       11,
		DeferredUsageConcurrency: 2,
	}
	plugin, err = Init(context.Background(), &Config{Writer: writer}, testLogger{}, newTestStore(t), nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() with writer error = %v", err)
	}
	if plugin.writerConfig != writer.WithDefaults() {
		t.Fatalf("writerConfig = %#v, want %#v", plugin.writerConfig, writer.WithDefaults())
	}
	if cap(plugin.writeQueue) != writer.WriteQueueCapacity {
		t.Fatalf("writeQueue cap = %d, want %d", cap(plugin.writeQueue), writer.WriteQueueCapacity)
	}
	if cap(plugin.deferredUsageSem) != writer.DeferredUsageConcurrency {
		t.Fatalf("deferredUsageSem cap = %d, want %d", cap(plugin.deferredUsageSem), writer.DeferredUsageConcurrency)
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() with writer error = %v", err)
	}
}

func TestProcessBatchKeepsRicherDuplicateLogEntry(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer func() {
		if err := plugin.Cleanup(); err != nil {
			t.Fatalf("Cleanup() error = %v", err)
		}
	}()

	id := "duplicate-with-usage"
	cost := 0.000319
	first := makeTestLog(id)
	first.Status = "success"
	first.Cost = &cost

	second := makeTestLog(id)
	second.Status = "success"
	second.TokenUsageParsed = &schemas.BifrostLLMUsage{
		PromptTokens:     10,
		CompletionTokens: 3,
		TotalTokens:      13,
	}
	second.PromptTokens = 10
	second.CompletionTokens = 3
	second.TotalTokens = 13

	plugin.processBatch(context.Background(), []*writeQueueEntry{
		{log: first},
		{log: second},
	})

	got, err := store.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.Cost == nil || *got.Cost != cost {
		t.Fatalf("stored cost = %v, want %v", got.Cost, cost)
	}
	if got.TokenUsageParsed == nil {
		t.Fatalf("stored token usage is nil")
	}
	if got.TokenUsageParsed.TotalTokens != 13 {
		t.Fatalf("stored total tokens = %d, want 13", got.TokenUsageParsed.TotalTokens)
	}
}

func TestProcessBatchRepairsBillingAcrossBatchBoundary(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer func() {
		if err := plugin.Cleanup(); err != nil {
			t.Fatalf("Cleanup() error = %v", err)
		}
	}()

	id := "duplicate-across-batches"
	cost := 0.00184
	first := makeTestLog(id)
	first.Status = "success"
	first.Cost = &cost
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: first}})

	second := makeTestLog(id)
	second.Status = "success"
	second.TokenUsageParsed = &schemas.BifrostLLMUsage{
		PromptTokens:     1600,
		CompletionTokens: 120,
		TotalTokens:      1720,
		Cost:             &schemas.BifrostCost{InputCost: 0.001, OutputCost: 0.0008, AdditionalCost: 0.00004, TotalCost: cost},
	}
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: second}})

	got, err := store.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.Cost == nil || *got.Cost != cost {
		t.Fatalf("stored cost = %v, want %v", got.Cost, cost)
	}
	if got.TokenUsageParsed == nil || got.TokenUsageParsed.TotalTokens != 1720 {
		t.Fatalf("stored token usage = %+v, want 1720 total tokens", got.TokenUsageParsed)
	}
	if got.InputCost != 0.001 || got.OutputCost != 0.0008 || got.AdditionalCost != 0.00004 {
		t.Fatalf("stored cost split = %g/%g/%g, want 0.001/0.0008/0.00004", got.InputCost, got.OutputCost, got.AdditionalCost)
	}
}

// When the usage-bearing duplicate wins the merge, the cost copied in from the
// priced duplicate must bring its input/output/additional split; copying the total
// alone makes serialization attribute all of it to input.
func TestProcessBatchMergeKeepsDuplicateCostSplit(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer func() {
		if err := plugin.Cleanup(); err != nil {
			t.Fatalf("Cleanup() error = %v", err)
		}
	}()

	cost := 0.003
	priced := makeTestLog("")
	priced.Status = ""
	priced.Cost = &cost
	priced.InputCost = 0.002
	priced.OutputCost = 0.0009
	priced.AdditionalCost = 0.0001

	counted := makeTestLog("")
	counted.TokenUsageParsed = &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}

	for name, order := range map[string][]*logstore.Log{
		"priced first":  {priced, counted},
		"counted first": {counted, priced},
	} {
		t.Run(name, func(t *testing.T) {
			rowID := "duplicate-cost-split-" + strings.ReplaceAll(name, " ", "-")
			batch := make([]*writeQueueEntry, 0, len(order))
			for _, log := range order {
				batch = append(batch, &writeQueueEntry{log: cloneTestLog(log, rowID)})
			}
			plugin.processBatch(context.Background(), batch)

			got, err := store.FindByID(context.Background(), rowID)
			if err != nil {
				t.Fatalf("FindByID() error = %v", err)
			}
			if got.Cost == nil || *got.Cost != cost {
				t.Fatalf("stored cost = %v, want %v", got.Cost, cost)
			}
			if got.TokenUsageParsed == nil || got.TokenUsageParsed.TotalTokens != 13 {
				t.Fatalf("stored token usage = %+v, want 13 total tokens", got.TokenUsageParsed)
			}
			if got.InputCost != 0.002 || got.OutputCost != 0.0009 || got.AdditionalCost != 0.0001 {
				t.Fatalf("stored cost split = %g/%g/%g, want 0.002/0.0009/0.0001", got.InputCost, got.OutputCost, got.AdditionalCost)
			}
		})
	}
}

func cloneTestLog(src *logstore.Log, id string) *logstore.Log {
	clone := *src
	clone.ID = id
	if src.TokenUsageParsed != nil {
		usage := *src.TokenUsageParsed
		clone.TokenUsageParsed = &usage
	}
	return &clone
}

// flushCountingStore records the processing-row Flush calls the cleanup tick
// issues and lets a test pretend another node holds the migration lock.
type flushCountingStore struct {
	logstore.LogStore
	mu        sync.Mutex
	flushes   int
	migrating bool
}

func (s *flushCountingStore) setMigrating(v bool) { s.mu.Lock(); s.migrating = v; s.mu.Unlock() }

func (s *flushCountingStore) MigrationInProgress(context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.migrating
}

func (s *flushCountingStore) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.flushes }

func (s *flushCountingStore) Flush(context.Context, time.Time) error {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
	return nil
}

func (s *flushCountingStore) FlushMCPToolLogs(context.Context, time.Time) error {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
	return nil
}

func (s *flushCountingStore) FlushAgentLogs(context.Context, time.Time) error {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
	return nil
}

// TestCleanupTickSkipsFlushWhileMigrating pins that the periodic
// processing-row deletes stay off the logs table while another node migrates,
// and run as usual (all three tables) once the lock is released.
func TestCleanupTickSkipsFlushWhileMigrating(t *testing.T) {
	store := &flushCountingStore{LogStore: newTestStore(t)}
	plugin := &LoggerPlugin{ctx: context.Background(), store: store, logger: testLogger{}}

	store.setMigrating(true)
	plugin.cleanupOldProcessingLogs()
	if got := store.count(); got != 0 {
		t.Fatalf("expected no Flush calls while the migration lock is held, got %d", got)
	}

	store.setMigrating(false)
	plugin.cleanupOldProcessingLogs()
	if got := store.count(); got != 3 {
		t.Fatalf("expected the LLM, MCP and A2A flushes once the lock cleared, got %d", got)
	}
}

// billingRepairStore lets a test fail the duplicate-billing repair UPDATE, and
// lets a test fail inserts transiently, so the writer's completeness tracking can
// be checked against what actually reached the store.
type billingRepairStore struct {
	logstore.LogStore
	failInserts atomic.Bool
	// insertErr is the error failInserts returns; a refused connection when nil.
	insertErr error
	// onUpdate, when set, runs before each Update and may return an error in its place.
	onUpdate func(ctx context.Context) error
}

func (s *billingRepairStore) BatchCreateIfNotExists(ctx context.Context, entries []*logstore.Log) error {
	if s.failInserts.Load() {
		if s.insertErr != nil {
			return s.insertErr
		}
		return fmt.Errorf("write logs: %w", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
	}
	return s.LogStore.BatchCreateIfNotExists(ctx, entries)
}

func (s *billingRepairStore) Update(ctx context.Context, id string, entry any) error {
	if s.onUpdate != nil {
		if err := s.onUpdate(ctx); err != nil {
			return err
		}
	}
	return s.LogStore.Update(ctx, id, entry)
}

func writerConfigWithBatchSize(size int) *logstore.WriterConfig {
	config := validWriterConfig()
	config.MaxBatchSize = size
	return &config
}

func pricedTestLog(id string, cost float64) *logstore.Log {
	log := makeTestLog(id)
	log.Cost = &cost
	log.InputCost = cost * 0.6
	log.OutputCost = cost * 0.4
	return log
}

func countedTestLog(id string) *logstore.Log {
	log := makeTestLog(id)
	log.TokenUsageParsed = &schemas.BifrostLLMUsage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42}
	return log
}

// A row the store never kept must not count as persisted billing: if it did, a
// sparse duplicate that does get inserted would be treated as already carrying
// the dropped row's cost and usage, and a later rich duplicate (a conflict-safe
// no-op insert) would skip the repair that is now the only way to record them.
func TestProcessBatchDroppedRowDoesNotMarkBillingPersisted(t *testing.T) {
	const cost = 0.0042
	for _, c := range []struct {
		name      string
		insertErr error
	}{
		{"transient outage", nil},
		{"row refused by both per-row attempts", errors.New("value too long for type character varying(255)")},
		{"single row over statement_timeout", fmt.Errorf("insert: %w", sqlStateErr("57014"))},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &billingRepairStore{LogStore: newTestStore(t), insertErr: c.insertErr}
			plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
			if err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			t.Cleanup(func() { _ = plugin.Cleanup() })

			const id = "dropped-rich-row"
			rich := pricedTestLog(id, cost)
			rich.TokenUsageParsed = &schemas.BifrostLLMUsage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42}

			store.failInserts.Store(true)
			plugin.processBatch(context.Background(), []*writeQueueEntry{{log: cloneTestLog(rich, id)}})
			store.failInserts.Store(false)
			if dropped := plugin.droppedRequests.Load(); dropped != 1 {
				t.Fatalf("expected the rich row to be dropped, got %d drops", dropped)
			}

			sparse := makeTestLog(id)
			plugin.processBatch(context.Background(), []*writeQueueEntry{{log: sparse}})
			plugin.processBatch(context.Background(), []*writeQueueEntry{{log: cloneTestLog(rich, id)}})

			got, err := store.FindByID(context.Background(), id)
			if err != nil {
				t.Fatalf("FindByID() error = %v", err)
			}
			if got.Cost == nil || *got.Cost != cost {
				t.Fatalf("stored cost = %v, want %v: the dropped row's billing was cached as persisted", got.Cost, cost)
			}
			if got.TokenUsageParsed == nil || got.TokenUsageParsed.TotalTokens != 42 {
				t.Fatalf("stored token usage = %+v, want 42 total tokens", got.TokenUsageParsed)
			}
		})
	}
}

// Control for the test above: a row the store did keep stays cached, so a later
// duplicate with the same billing issues no UPDATE.
func TestPersistedRowNeedsNoRepairFromAnEqualDuplicate(t *testing.T) {
	store := &billingRepairStore{LogStore: newTestStore(t)}
	var updates atomic.Int32
	store.onUpdate = func(context.Context) error {
		updates.Add(1)
		return nil
	}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	const id = "persisted-rich-row"
	rich := pricedTestLog(id, 0.0042)
	rich.TokenUsageParsed = &schemas.BifrostLLMUsage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42}
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: cloneTestLog(rich, id)}})
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: cloneTestLog(rich, id)}})
	if n := updates.Load(); n != 0 {
		t.Fatalf("expected no repair UPDATE for an equal duplicate, got %d", n)
	}
}

// A duplicate whose insert landed but whose billing repair was interrupted by
// the batch context ending (Cleanup cancelling the writer) is not finished: it
// must be handed back for the drain, which reruns the no-op insert and the
// repair, instead of the repair failure only being logged.
func TestProcessBatchHandsBackDuplicateWhoseRepairWasInterrupted(t *testing.T) {
	store := &billingRepairStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	const id = "interrupted-repair"
	const cost = 0.0031
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: pricedTestLog(id, cost)}})

	batchCtx, cancelBatch := context.WithCancel(context.Background())
	store.onUpdate = func(ctx context.Context) error {
		if ctx == batchCtx {
			// Shutdown lands while the repair is in flight.
			cancelBatch()
			return ctx.Err()
		}
		return nil
	}
	var callbacks atomic.Int32
	entry := &writeQueueEntry{log: countedTestLog(id), callback: func(*logstore.Log) { callbacks.Add(1) }}
	left := plugin.processBatch(batchCtx, []*writeQueueEntry{entry})
	if len(left) != 1 || left[0] != entry {
		t.Fatalf("expected the duplicate with the interrupted repair handed back, got %d entries", len(left))
	}

	if left := plugin.processBatch(context.Background(), left); len(left) != 0 {
		t.Fatalf("drain retry left %d entries", len(left))
	}
	got, err := store.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.TokenUsageParsed == nil || got.TokenUsageParsed.TotalTokens != 42 {
		t.Fatalf("stored token usage = %+v, want 42 total tokens after the drain's repair", got.TokenUsageParsed)
	}
	if got.Cost == nil || *got.Cost != cost {
		t.Fatalf("stored cost = %v, want %v", got.Cost, cost)
	}
	waitFor(t, "the drain's callback", func() bool { return callbacks.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := callbacks.Load(); n != 1 {
		t.Fatalf("callback ran %d times, want once (only after the repair landed)", n)
	}
	if dropped := plugin.droppedRequests.Load(); dropped != 0 {
		t.Fatalf("interrupted repair must not count as dropped, got %d", dropped)
	}
}

// TestInterruptedRepairSurvivesEvictionUntilItsRetry pins that a duplicate whose
// billing repair was interrupted keeps its repair state through the recent-billing
// cache's eviction, so the drain's retry still writes the UPDATE.
func TestInterruptedRepairSurvivesEvictionUntilItsRetry(t *testing.T) {
	store := &billingRepairStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{Writer: writerConfigWithBatchSize(1)}, testLogger{}, store, nil, nil, nil) // the cache keeps two entries
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	const id = "interrupted-then-evicted"
	const cost = 0.0031
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: pricedTestLog(id, cost)}})
	batchCtx, cancelBatch := context.WithCancel(context.Background())
	store.onUpdate = func(ctx context.Context) error {
		if ctx == batchCtx {
			cancelBatch()
			return ctx.Err()
		}
		return nil
	}
	left := plugin.processBatch(batchCtx, []*writeQueueEntry{{log: countedTestLog(id)}})
	if len(left) != 1 {
		t.Fatalf("expected the interrupted duplicate handed back, got %d", len(left))
	}
	// Newer rows push the cache past its bound before the drain retries.
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: makeTestLog("newer-1")}, {log: makeTestLog("newer-2")}})

	if left := plugin.processBatch(context.Background(), left); len(left) != 0 {
		t.Fatalf("drain retry left %d entries", len(left))
	}
	got, err := store.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.TokenUsageParsed == nil || got.TokenUsageParsed.TotalTokens != 42 {
		t.Fatalf("stored token usage = %+v, want 42 total tokens after the retry's repair", got.TokenUsageParsed)
	}
}

// A repair running when Cleanup cancels the writer ends with the batch context, and
// the drain finishes it: shutdown neither waits on a wedged UPDATE nor loses it.
func TestCleanupRetriesRepairInterruptedByShutdown(t *testing.T) {
	store := &billingRepairStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{Writer: writerConfigWithBatchSize(1)}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	inRepair, wedged := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	store.onUpdate = func(ctx context.Context) error {
		if attempts.Add(1) > 1 {
			return nil
		}
		close(inRepair)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wedged:
			return errors.New("repair UPDATE outlived its test")
		}
	}

	const id = "repair-during-shutdown"
	const cost = 0.0031
	plugin.enqueueLogEntry(pricedTestLog(id, cost), nil)
	waitFor(t, "the priced row", func() bool {
		_, err := store.FindByID(context.Background(), id)
		return err == nil
	})
	var callbacks atomic.Int32
	plugin.enqueueLogEntry(countedTestLog(id), func(*logstore.Log) { callbacks.Add(1) })
	select {
	case <-inRepair:
	case <-time.After(3 * time.Second):
		t.Fatal("the duplicate's repair never reached the store")
	}

	cleaned := make(chan struct{})
	go func() {
		defer close(cleaned)
		_ = plugin.Cleanup()
	}()
	select {
	case <-cleaned:
	case <-time.After(3 * time.Second):
		close(wedged)
		<-cleaned
		t.Fatal("Cleanup stayed behind a repair UPDATE that ignored the batch context")
	}

	if n := attempts.Load(); n != 2 {
		t.Fatalf("expected the drain to retry the repair once, got %d attempts", n)
	}
	got, err := store.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.TokenUsageParsed == nil || got.TokenUsageParsed.TotalTokens != 42 {
		t.Fatalf("stored token usage = %+v, want the drain's repair", got.TokenUsageParsed)
	}
	if got.Cost == nil || *got.Cost != cost {
		t.Fatalf("stored cost = %v, want %v", got.Cost, cost)
	}
	waitFor(t, "the drain's callback", func() bool { return callbacks.Load() == 1 })
	if dropped := plugin.droppedRequests.Load(); dropped != 0 {
		t.Fatalf("interrupted repair must not count as dropped, got %d", dropped)
	}
}

// The repair and its retry against Postgres: the UPDATE writes usage and the split
// columns, a cancelled one leaves the row alone, and the retry finishes it.
func TestInterruptedRepairRetriesOnPostgres(t *testing.T) {
	cfg := testPostgresConnConfig()
	openTestPostgres(t, cfg)
	store := &billingRepairStore{LogStore: newTestPostgresStore(t, cfg)}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	id := fmt.Sprintf("postgres-repair-%d", time.Now().UnixNano())
	const cost = 0.0031
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: pricedTestLog(id, cost)}})
	batchCtx, cancelBatch := context.WithCancel(context.Background())
	defer cancelBatch()
	store.onUpdate = func(ctx context.Context) error {
		if ctx == batchCtx {
			cancelBatch()
		}
		return nil // the store sees the cancelled context itself
	}
	left := plugin.processBatch(batchCtx, []*writeQueueEntry{{log: countedTestLog(id)}})
	if len(left) != 1 {
		t.Fatalf("expected the interrupted duplicate handed back, got %d", len(left))
	}
	before, err := store.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if before.TotalTokens != 0 {
		t.Fatalf("a cancelled repair wrote %d tokens", before.TotalTokens)
	}

	if left := plugin.processBatch(context.Background(), left); len(left) != 0 {
		t.Fatalf("drain retry left %d entries", len(left))
	}
	got, err := store.FindByID(context.Background(), id)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.TotalTokens != 42 || got.TokenUsageParsed == nil || got.TokenUsageParsed.TotalTokens != 42 {
		t.Fatalf("stored usage = %d/%+v, want 42 total tokens", got.TotalTokens, got.TokenUsageParsed)
	}
	if got.Cost == nil || math.Abs(*got.Cost-cost) > 1e-12 {
		t.Fatalf("stored cost = %v, want %v", got.Cost, cost)
	}
	if math.Abs(got.InputCost-cost*0.6) > 1e-12 || math.Abs(got.OutputCost-cost*0.4) > 1e-12 {
		t.Fatalf("stored split %g/%g, want %g/%g", got.InputCost, got.OutputCost, cost*0.6, cost*0.4)
	}
	if len(plugin.recentLogBillingPinned) != 0 {
		t.Fatalf("pin outlived the retry: %v", plugin.recentLogBillingPinned)
	}
}

// A repair the store refuses, as opposed to one ctx interrupted, has no retry coming:
// it logs and moves on, and its entry goes back to being evictable.
func TestRefusedRepairIsNotHandedBackOrPinned(t *testing.T) {
	store := &billingRepairStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{Writer: writerConfigWithBatchSize(1)}, testLogger{}, store, nil, nil, nil) // the cache keeps two entries
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	const id = "refused-after-interruption"
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: pricedTestLog(id, 0.0031)}})
	batchCtx, cancelBatch := context.WithCancel(context.Background())
	store.onUpdate = func(ctx context.Context) error {
		if ctx == batchCtx {
			cancelBatch()
			return ctx.Err()
		}
		return errors.New("constraint violation")
	}
	left := plugin.processBatch(batchCtx, []*writeQueueEntry{{log: countedTestLog(id)}})
	if len(left) != 1 {
		t.Fatalf("expected the interrupted duplicate handed back, got %d", len(left))
	}
	if !plugin.recentLogBillingPinned[id] {
		t.Fatal("the interrupted duplicate's state must be pinned for its retry")
	}

	if left := plugin.processBatch(context.Background(), left); len(left) != 0 {
		t.Fatalf("a refused repair must not be handed back, got %d", len(left))
	}
	if len(plugin.recentLogBillingPinned) != 0 {
		t.Fatalf("a refused repair stays pinned: %v", plugin.recentLogBillingPinned)
	}
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: makeTestLog("newer-1")}, {log: makeTestLog("newer-2")}, {log: makeTestLog("newer-3")}})
	if _, cached := plugin.recentLogBilling[id]; cached {
		t.Fatal("the refused entry was never evicted")
	}
}

// A retry the store drops at its no-op insert never reaches the repair; the drop is what
// settles it, so its pin goes with it.
func TestDroppedRetryReleasesItsPin(t *testing.T) {
	store := &billingRepairStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	const id = "dropped-after-interruption"
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: pricedTestLog(id, 0.0031)}})
	batchCtx, cancelBatch := context.WithCancel(context.Background())
	defer cancelBatch()
	store.onUpdate = func(ctx context.Context) error {
		cancelBatch()
		return ctx.Err()
	}
	left := plugin.processBatch(batchCtx, []*writeQueueEntry{{log: countedTestLog(id)}})
	if len(left) != 1 || !plugin.recentLogBillingPinned[id] {
		t.Fatalf("expected one interrupted, pinned duplicate, got %d handed back, pins %v", len(left), plugin.recentLogBillingPinned)
	}

	store.failInserts.Store(true)
	if left := plugin.processBatch(context.Background(), left); len(left) != 0 {
		t.Fatalf("a dropped retry must not be handed back, got %d", len(left))
	}
	if len(plugin.recentLogBillingPinned) != 0 {
		t.Fatalf("a dropped retry stays pinned: %v", plugin.recentLogBillingPinned)
	}
	if dropped := plugin.droppedRequests.Load(); dropped != 1 {
		t.Fatalf("droppedRequests = %d, want 1", dropped)
	}
}

// Pins are bounded by the batch that was interrupted, the unpinned cache keeps its
// bound around them, and the end of the drain frees them even when the drain itself
// was interrupted.
func TestRepairPinsAreBoundedAndFreedByTheDrain(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	store := &billingRepairStore{LogStore: newTestStore(t)}
	plugin, err := Init(parent, &Config{Writer: writerConfigWithBatchSize(2)}, testLogger{}, store, nil, nil, nil) // the cache keeps four entries
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	const pinned = 6
	ids := make([]string, pinned)
	for i := range ids {
		ids[i] = fmt.Sprintf("pinned-%d", i)
		plugin.processBatch(context.Background(), []*writeQueueEntry{{log: pricedTestLog(ids[i], 0.001)}})
	}
	batchCtx, cancelBatch := context.WithCancel(context.Background())
	defer cancelBatch()
	store.onUpdate = func(ctx context.Context) error {
		cancelBatch()
		return ctx.Err()
	}
	batch := make([]*writeQueueEntry, 0, pinned)
	for _, id := range ids {
		batch = append(batch, &writeQueueEntry{log: countedTestLog(id)})
	}
	// Ids evicted by the bound above read as first copies; only the cached ones can
	// be interrupted, which is the most one batch can pin.
	left := plugin.processBatch(batchCtx, batch)
	if got := len(plugin.recentLogBillingPinned); got != len(left) || got == 0 {
		t.Fatalf("pinned %d entries for %d handed back", got, len(left))
	}
	store.onUpdate = nil
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: makeTestLog("newer-1")}, {log: makeTestLog("newer-2")}})
	if got, bound := len(plugin.recentLogBillingOrder), 4+len(left); got > bound {
		t.Fatalf("cache holds %d entries, bound is %d", got, bound)
	}
	if len(plugin.recentLogBilling) != len(plugin.recentLogBillingOrder) {
		t.Fatalf("cache map has %d entries, order has %d", len(plugin.recentLogBilling), len(plugin.recentLogBillingOrder))
	}

	// The drain retries under a context that is already over.
	cancelParent()
	<-plugin.batchWriterDone
	for _, entry := range left {
		plugin.writeQueue <- entry
	}
	if err := plugin.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if plugin.recentLogBillingPinned != nil {
		t.Fatalf("pins outlived the drain: %v", plugin.recentLogBillingPinned)
	}
	if dropped := plugin.droppedRequests.Load(); dropped != int64(len(left)) {
		t.Fatalf("dropped = %d, want the %d rows the drain could not finish", dropped, len(left))
	}
}

// The usage-bearing duplicate wins compaction and the priced one's split lives only
// on its parsed breakdown, which is the shape a row has before serialization.
func TestProcessBatchMergeKeepsBreakdownOfPricedDuplicate(t *testing.T) {
	breakdown := &schemas.BifrostCost{InputCost: 0.002, OutputCost: 0.0009, AdditionalCost: 0.0001, TotalCost: 0.003}
	details := map[string]func() *logstore.Log{
		"parsed usage": func() *logstore.Log {
			log := makeTestLog("")
			log.TokenUsageParsed = &schemas.BifrostLLMUsage{
				PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13,
				CompletionTokensDetails: &schemas.ChatCompletionTokensDetails{ReasoningTokens: 2},
			}
			return log
		},
		"usage in columns only": func() *logstore.Log {
			log := makeTestLog("")
			log.PromptTokens, log.CompletionTokens, log.TotalTokens = 10, 3, 13
			return log
		},
	}
	for name, counted := range details {
		for _, order := range []string{"priced first", "counted first"} {
			t.Run(name+"/"+order, func(t *testing.T) {
				store := newTestStore(t)
				plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
				if err != nil {
					t.Fatalf("Init() error = %v", err)
				}
				t.Cleanup(func() { _ = plugin.Cleanup() })

				cost := breakdown.TotalCost
				priced := makeTestLog("priced-breakdown")
				priced.Status = ""
				priced.Cost = &cost
				priced.TokenUsageParsed = &schemas.BifrostLLMUsage{Cost: breakdown}
				usageRow := counted()
				usageRow.ID = priced.ID
				sharedUsage := usageRow.TokenUsageParsed

				batch := []*writeQueueEntry{{log: priced}, {log: usageRow}}
				if order == "counted first" {
					batch[0], batch[1] = batch[1], batch[0]
				}
				plugin.processBatch(context.Background(), batch)

				got, err := store.FindByID(context.Background(), priced.ID)
				if err != nil {
					t.Fatalf("FindByID() error = %v", err)
				}
				if got.Cost == nil || *got.Cost != cost {
					t.Fatalf("stored cost = %v, want %v", got.Cost, cost)
				}
				if got.InputCost != 0.002 || got.OutputCost != 0.0009 || got.AdditionalCost != 0.0001 {
					t.Fatalf("stored cost split = %g/%g/%g, want 0.002/0.0009/0.0001", got.InputCost, got.OutputCost, got.AdditionalCost)
				}
				if got.TotalTokens != 13 || got.PromptTokens != 10 || got.CompletionTokens != 3 {
					t.Fatalf("stored tokens = %d/%d/%d, want 10/3/13", got.PromptTokens, got.CompletionTokens, got.TotalTokens)
				}
				if sharedUsage != nil && sharedUsage.Cost != nil {
					t.Fatal("the merge wrote the breakdown through a usage pointer the row's producer still holds")
				}
				if name == "parsed usage" {
					if got.TokenUsageParsed == nil || !reflect.DeepEqual(got.TokenUsageParsed.CompletionTokensDetails, &schemas.ChatCompletionTokensDetails{ReasoningTokens: 2}) {
						t.Fatalf("usage details changed: %+v", got.TokenUsageParsed)
					}
					if got.TokenUsageParsed.Cost == nil || got.TokenUsageParsed.Cost.OutputCost != 0.0009 {
						t.Fatalf("breakdown not on the usage: %+v", got.TokenUsageParsed.Cost)
					}
				}
			})
		}
	}
}

// An empty usage object on the winning row is no usage: the priced duplicate's breakdown
// must not make it read as filled and cost the row its token counts.
func TestProcessBatchMergeFillsEmptyUsageFromPricedDuplicate(t *testing.T) {
	store := newTestStore(t)
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	cost := 0.003
	priced := makeTestLog("empty-usage-winner")
	priced.Status = ""
	priced.Cost = &cost
	priced.TokenUsageParsed = &schemas.BifrostLLMUsage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42,
		Cost: &schemas.BifrostCost{InputCost: 0.002, OutputCost: 0.001, TotalCost: cost}}
	empty := makeTestLog(priced.ID)
	empty.TokenUsageParsed = &schemas.BifrostLLMUsage{}

	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: empty}, {log: priced}})

	got, err := store.FindByID(context.Background(), priced.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if got.TotalTokens != 42 || got.PromptTokens != 40 || got.CompletionTokens != 2 {
		t.Fatalf("stored tokens = %d/%d/%d, want 40/2/42", got.PromptTokens, got.CompletionTokens, got.TotalTokens)
	}
	if got.InputCost != 0.002 || got.OutputCost != 0.001 {
		t.Fatalf("stored cost split = %g/%g, want 0.002/0.001", got.InputCost, got.OutputCost)
	}
}

// billingBenchStore accepts every write without doing one, so a benchmark sees the
// plugin's own bookkeeping.
type billingBenchStore struct{ logstore.LogStore }

func (billingBenchStore) BatchCreateIfNotExists(context.Context, []*logstore.Log) error { return nil }
func (billingBenchStore) Update(context.Context, string, any) error                     { return nil }

// BenchmarkProcessBatchBillingCache is the batch writer's per-batch cost at a full
// recent-billing cache, which is where eviction runs on every batch. Not on the
// request path; it bounds what the writer goroutine pays per 100 rows.
func BenchmarkProcessBatchBillingCache(b *testing.B) {
	plugin := &LoggerPlugin{logger: testLogger{}, store: billingBenchStore{}, writerConfig: logstore.WriterConfig{MaxBatchSize: 100}}
	batch := make([]*writeQueueEntry, 100)
	for i := range batch {
		batch[i] = &writeQueueEntry{log: &logstore.Log{}}
	}
	ctx := context.Background()
	next := 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, entry := range batch {
			next++
			entry.log.ID = "row-" + strconv.Itoa(next)
		}
		plugin.processBatch(ctx, batch)
	}
}

// BenchmarkCompactLogsForInsert prices a batch of two-copy rows, the merge path.
func BenchmarkCompactLogsForInsert(b *testing.B) {
	cost := 0.003
	usage := &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13}
	breakdown := &schemas.BifrostLLMUsage{Cost: &schemas.BifrostCost{InputCost: 0.002, OutputCost: 0.001, TotalCost: cost}}
	logs := make([]*logstore.Log, 0, 200)
	for i := 0; i < 100; i++ {
		id := "row-" + strconv.Itoa(i)
		logs = append(logs, &logstore.Log{ID: id, Cost: &cost, TokenUsageParsed: breakdown}, &logstore.Log{ID: id, Status: "success", TokenUsageParsed: usage})
	}
	batch := make([]*logstore.Log, len(logs))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j, log := range logs {
			copied := *log
			batch[j] = &copied
		}
		compactLogsForInsert(batch)
	}
}
