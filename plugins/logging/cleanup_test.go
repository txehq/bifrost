package logging

import (
	"context"
	"fmt"
	"net"
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
	// onUpdate, when set, runs before each Update and may return an error in its place.
	onUpdate func(ctx context.Context) error
}

func (s *billingRepairStore) BatchCreateIfNotExists(ctx context.Context, entries []*logstore.Log) error {
	if s.failInserts.Load() {
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
	store := &billingRepairStore{LogStore: newTestStore(t)}
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })

	const id = "dropped-rich-row"
	const cost = 0.0042
	rich := pricedTestLog(id, cost)
	rich.TokenUsageParsed = &schemas.BifrostLLMUsage{PromptTokens: 40, CompletionTokens: 2, TotalTokens: 42}

	store.failInserts.Store(true)
	plugin.processBatch(context.Background(), []*writeQueueEntry{{log: rich}})
	store.failInserts.Store(false)
	if dropped := plugin.droppedRequests.Load(); dropped != 1 {
		t.Fatalf("expected the rich row to be dropped after transient failures, got %d drops", dropped)
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
	plugin, err := Init(context.Background(), &Config{}, testLogger{}, store, nil, nil, nil)
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = plugin.Cleanup() })
	plugin.writerConfig.MaxBatchSize = 1 // the cache keeps two entries

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
