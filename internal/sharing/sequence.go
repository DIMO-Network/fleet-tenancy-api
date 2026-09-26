package sharing

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/rs/zerolog"
)

// sequenceSnooze is how long a held-back job waits before checking again. A
// share or revoke takes seconds to a few minutes, and the callers poll every
// few seconds, so this bounds the added latency without spinning.
const sequenceSnooze = 10 * time.Second

// vehicleSequencer runs this queue's jobs one vehicle at a time, in the order
// they were queued.
//
// Every job here — a share, a revoke, a shared operation — sends a UserOp from
// the vehicle owner's kernel, and each one overwrites or depends on what the
// last one left. Nothing else orders them: the queue runs maxWorkers jobs at
// once, a job reads the kernel nonce when it builds its operation, and a share
// spends seconds publishing its SACD document before it sends while a revoke
// sends at once. So a revoke queued AFTER an upgrade could land BEFORE it, and
// the upgrade would then re-grant everything the customer had just been told
// was revoked. That is the case the fleet-lite UI reaches most easily: it
// stops waiting on a slow upgrade after two minutes and offers Revoke.
//
// A job is therefore held — snoozed, which River does not count as an attempt
// — while any EARLIER job for the same token id is still unfinished. Queue
// order is job id order, so the lowest unfinished job for a vehicle always
// runs and there is no cycle to deadlock on. A job left "running" by a crashed
// process holds the vehicle until River's stuck-job rescue settles it, which
// is the right answer: nobody knows yet whether its operation landed.
//
// Installed as River worker middleware so it covers every job kind on the
// client, including kinds added later, without each worker having to remember.
type vehicleSequencer struct {
	river.MiddlewareDefaults

	logger  zerolog.Logger
	pending func(ctx context.Context, jobID int64, tokenID string) (bool, error)
}

var _ rivertype.WorkerMiddleware = (*vehicleSequencer)(nil)

// earlierUnfinishedJobQuery reports whether a job queued before $2 for the same
// vehicle is still unfinished. tokenId is compared as text so a job whose args
// lack it (NULL) simply never matches.
const earlierUnfinishedJobQuery = `
SELECT EXISTS (
	SELECT 1 FROM river_job
	 WHERE queue = $1
	   AND id < $2
	   AND state IN ('available', 'pending', 'retryable', 'running', 'scheduled')
	   AND args->>'tokenId' = $3
)`

func newVehicleSequencer(pool *pgxpool.Pool, logger zerolog.Logger) *vehicleSequencer {
	return &vehicleSequencer{
		logger: logger.With().Str("component", "vehicle-sequencer").Logger(),
		pending: func(ctx context.Context, jobID int64, tokenID string) (bool, error) {
			var exists bool
			err := pool.QueryRow(ctx, earlierUnfinishedJobQuery, QueueName, jobID, tokenID).Scan(&exists)
			return exists, err
		},
	}
}

// Work holds the job back while an earlier job for its vehicle is unfinished.
func (s *vehicleSequencer) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	tokenID, ok := jobTokenID(job.EncodedArgs)
	if !ok {
		// Not about a vehicle: nothing to order it against.
		return doInner(ctx)
	}
	blocked, err := s.pending(ctx, job.ID, tokenID)
	if err != nil {
		// Not knowing is not permission to run out of order. Snooze rather than
		// fail: a failed share or revoke is a customer-facing error, and the
		// database being briefly unreachable is not their problem.
		s.logger.Error().Err(err).Int64("job_id", job.ID).Str("token_id", tokenID).
			Msg("could not check for earlier jobs on this vehicle; holding the job")
		return river.JobSnooze(sequenceSnooze)
	}
	if blocked {
		s.logger.Info().Int64("job_id", job.ID).Str("kind", job.Kind).Str("token_id", tokenID).
			Msg("an earlier job for this vehicle is unfinished; holding this one")
		return river.JobSnooze(sequenceSnooze)
	}
	return doInner(ctx)
}

// jobTokenID reads the vehicle a job is about from its encoded args, rendered
// the way Postgres' ->> renders a JSON number.
func jobTokenID(encodedArgs []byte) (string, bool) {
	var args struct {
		TokenID *json.Number `json:"tokenId"`
	}
	if err := json.Unmarshal(encodedArgs, &args); err != nil || args.TokenID == nil {
		return "", false
	}
	id, err := strconv.ParseInt(args.TokenID.String(), 10, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(id, 10), true
}
