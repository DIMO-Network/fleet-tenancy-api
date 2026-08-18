package sharing

import (
	"context"
	"fmt"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/rs/zerolog"
)

// QueueName isolates share jobs from anything this service queues later. River
// needs a queue configured before a worker can pull from it, and naming it now
// means a future queue does not silently share this one's worker budget.
const QueueName = "vehicle_sharing"

// maxWorkers is deliberately small. Every job here is dominated by waiting on
// a bundler, not by CPU or by the database, so a large pool buys nothing and
// costs connections — and this process also serves GET /v1/authz, which both
// apps call on every request and fail closed on. Sharing must not be able to
// starve it. Raise this only with evidence that share jobs are queueing.
const maxWorkers = 4

// maxPoolConns bounds the separate pgx pool River needs (the rest of the
// service is on database/sql via shared/pkg/db, which River cannot use). It is
// a second pool against the same database, so it is sized as small as River
// tolerates: the workers above, plus headroom for River's own leadership and
// notifier connections.
const maxPoolConns = 8

// rescueStuckJobsAfter re-runs jobs left "running" by a hard-killed worker.
// It must stay above the longest legitimate run — a share waits up to
// receiptPollingDelaySeconds × receiptPollingRetries (5 minutes) for a
// receipt — or River rescues a job whose UserOp is still in flight and the
// grant is sent twice. Kaufmann learned this with a 40-minute value against a
// 30-minute worker timeout; the same margin applies at this scale.
const rescueStuckJobsAfter = 20 * time.Minute

// Queue owns the River client and the pgx pool behind it.
//
// It exists so main() has one thing to start and one thing to stop. Both are
// nil when sharing is unconfigured — see NewQueue.
type Queue struct {
	Client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
	logger zerolog.Logger
}

// NewQueue builds the River client for the supplied workers.
//
// Returns (nil, nil) in two supported states, both of which must leave the
// service running normally:
//
//   - Sharing is not configured. This service is what both apps fail closed
//     on, and a missing bundler URL must not stop it answering /v1/authz.
//   - No workers are registered (workers == nil), which is every build until
//     the share worker lands in step 2 of docs/plans/05-vehicle-sharing.md.
//
// The second case is not merely an optimisation. river.Client.Start rejects an
// empty Workers bundle outright — "at least one Worker must be added to the
// Workers bundle", verified against River v0.31.0 and a live database — so
// building a client here with nothing registered would turn into a fatal error
// at startup and take the service down in exactly the environments where
// sharing IS configured. Nothing enqueues share jobs until
// that step anyway, so the queue is idle rather than backlogged.
//
// Callers check for a nil Queue rather than a nil error.
func NewQueue(ctx context.Context, logger *zerolog.Logger, settings *config.Settings, workers *river.Workers) (*Queue, error) {
	if !settings.SharingConfigured() {
		logger.Info().Msg("vehicle sharing not configured; job queue not started")
		return nil, nil
	}
	if workers == nil {
		logger.Info().Msg("vehicle sharing configured but no workers registered; job queue not started")
		return nil, nil
	}

	pool, err := newPool(ctx, settings)
	if err != nil {
		return nil, err
	}

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:               map[string]river.QueueConfig{QueueName: {MaxWorkers: maxWorkers}},
		Workers:              workers,
		RescueStuckJobsAfter: rescueStuckJobsAfter,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("river client: %w", err)
	}

	return &Queue{Client: client, pool: pool, logger: *logger}, nil
}

func newPool(ctx context.Context, settings *config.Settings) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(settings.DB.BuildConnectionString(true))
	if err != nil {
		return nil, fmt.Errorf("river pool config: %w", err)
	}
	cfg.MaxConns = maxPoolConns
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("river pool: %w", err)
	}
	return pool, nil
}

// Start begins processing. Safe to call on a nil Queue, which is what an
// unconfigured environment produces — the caller should not have to branch.
func (q *Queue) Start(ctx context.Context) error {
	if q == nil {
		return nil
	}
	q.logger.Info().Str("queue", QueueName).Int("max_workers", maxWorkers).
		Msg("starting vehicle-sharing job queue")
	if err := q.Client.Start(ctx); err != nil {
		return fmt.Errorf("start river client: %w", err)
	}
	return nil
}

// Stop drains in-flight jobs and closes the pool. Safe on a nil Queue.
//
// It takes its own context rather than reusing the cancelled shutdown context:
// River's Stop waits for running jobs to finish, and passing an
// already-cancelled context would abandon a UserOp mid-flight — the grant
// lands on-chain with nothing recorded about it.
func (q *Queue) Stop(ctx context.Context) error {
	if q == nil {
		return nil
	}
	defer q.pool.Close()
	if err := q.Client.Stop(ctx); err != nil {
		return fmt.Errorf("stop river client: %w", err)
	}
	return nil
}
