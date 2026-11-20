package logging

import (
	"strings"
	"testing"
	"time"

	"github.com/capsohq/bifrost/framework/logstore"
)

// validWriterConfig returns a WriterConfig that passes validation, so each test
// case below can vary exactly one field and attribute the failure to it.
func validWriterConfig() logstore.WriterConfig {
	return logstore.WriterConfig{
		MaxBatchSize:             10,
		BatchInterval:            "5s",
		MaxBatchBytes:            1024,
		WriteQueueCapacity:       100,
		DeferredUsageConcurrency: 2,
	}
}

// WriteQueueCapacity and DeferredUsageConcurrency are used directly as make()
// sizes in Init, so validation has to reject out-of-range values before the
// allocation rather than let a bad config.json take the process down at boot.
func TestValidateWriterConfigBounds(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*logstore.WriterConfig)
		wantErr string
	}{
		{
			name:   "valid config passes",
			mutate: func(c *logstore.WriterConfig) {},
		},
		{
			name:    "pending log ttl that is not a duration is rejected",
			mutate:  func(c *logstore.WriterConfig) { c.PendingLogTTL = "soon" },
			wantErr: "pending_log_ttl must be a valid Go duration",
		},
		{
			name:    "pending log ttl below the floor is rejected",
			mutate:  func(c *logstore.WriterConfig) { c.PendingLogTTL = "10ms" },
			wantErr: "pending_log_ttl must be at least",
		},
		{
			name:   "pending log ttl at the floor is accepted",
			mutate: func(c *logstore.WriterConfig) { c.PendingLogTTL = minPendingLogTTL.String() },
		},
		{
			name:    "write queue capacity above the cap is rejected",
			mutate:  func(c *logstore.WriterConfig) { c.WriteQueueCapacity = maxWriterQueueCapacity + 1 },
			wantErr: "write_queue_capacity must be at most",
		},
		{
			name:    "write queue capacity at the cap is accepted",
			mutate:  func(c *logstore.WriterConfig) { c.WriteQueueCapacity = maxWriterQueueCapacity },
			wantErr: "",
		},
		{
			name: "deferred usage concurrency above the cap is rejected",
			mutate: func(c *logstore.WriterConfig) {
				c.DeferredUsageConcurrency = maxWriterDeferredUsageConcurrency + 1
			},
			wantErr: "deferred_usage_concurrency must be at most",
		},
		{
			name: "deferred usage concurrency at the cap is accepted",
			mutate: func(c *logstore.WriterConfig) {
				c.DeferredUsageConcurrency = maxWriterDeferredUsageConcurrency
			},
			wantErr: "",
		},
		{
			// The overflow shape the allocation-size alert is about: a value this
			// large would ask make() for terabytes of channel buffer.
			name:    "absurd write queue capacity is rejected, not allocated",
			mutate:  func(c *logstore.WriterConfig) { c.WriteQueueCapacity = 1 << 40 },
			wantErr: "write_queue_capacity must be at most",
		},
		{
			name:    "zero write queue capacity is still rejected",
			mutate:  func(c *logstore.WriterConfig) { c.WriteQueueCapacity = 0 },
			wantErr: "write_queue_capacity must be greater than 0",
		},
		{
			name:    "negative deferred usage concurrency is still rejected",
			mutate:  func(c *logstore.WriterConfig) { c.DeferredUsageConcurrency = -1 },
			wantErr: "deferred_usage_concurrency must be greater than 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validWriterConfig()
			tt.mutate(&cfg)
			err := validateWriterConfig(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateWriterConfig() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateWriterConfig() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateWriterConfig() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestPendingLogTTLDefaultsAndSweepInterval (TXE-7328): an unset
// writer.pending_log_ttl keeps the 15-minute eviction and the one-minute sweep;
// a short one sweeps every half TTL, so the eviction path the harness pins is
// reached within a second or two.
func TestPendingLogTTLDefaultsAndSweepInterval(t *testing.T) {
	var unset *logstore.WriterConfig
	got, err := time.ParseDuration(unset.WithDefaults().PendingLogTTL)
	if err != nil || got != pendingLogTTL {
		t.Fatalf("default pending_log_ttl = %v (%v), want %v", got, err, pendingLogTTL)
	}
	if got := cleanupInterval(pendingLogTTL); got != maxCleanupInterval {
		t.Fatalf("cleanupInterval(default) = %v, want %v", got, maxCleanupInterval)
	}
	if got := cleanupInterval(time.Second); got != 500*time.Millisecond {
		t.Fatalf("cleanupInterval(1s) = %v, want 500ms", got)
	}
	if got := (&LoggerPlugin{}).pendingIdleTTL(); got != pendingLogTTL {
		t.Fatalf("pendingIdleTTL without Init = %v, want %v", got, pendingLogTTL)
	}
	if got := (&LoggerPlugin{pendingTTL: time.Second}).pendingIdleTTL(); got != time.Second {
		t.Fatalf("pendingIdleTTL = %v, want 1s", got)
	}
}
