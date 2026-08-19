package sharing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// ErrQueueUnavailable means sharing is not configured in this environment, so
// there is nothing to enqueue into. Callers turn it into a 503 rather than a
// 500: the request was fine, the feature is off here.
var ErrQueueUnavailable = errors.New("vehicle sharing is not available in this environment")

// Enqueue queues a share and returns its job id.
func (q *Queue) Enqueue(ctx context.Context, args ShareArgs) (int64, error) {
	if q == nil {
		return 0, ErrQueueUnavailable
	}
	res, err := q.Client.Insert(ctx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("enqueue share: %w", err)
	}
	return res.Job.ID, nil
}

// Status reports on a queued share.
//
// Scoped by tenant: the job id alone would otherwise let any caller read any
// tenant's share outcome, and job ids are sequential integers. The tenant is
// compared against the one stored in the job's own arguments, so the check
// cannot drift from what the job will actually do.
func (q *Queue) Status(ctx context.Context, tenantID string, jobID int64) (*models.ShareStatus, error) {
	if q == nil {
		return nil, ErrQueueUnavailable
	}
	job, err := q.Client.JobGet(ctx, jobID)
	if err != nil {
		if errors.Is(err, rivertype.ErrNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("read share job %d: %w", jobID, err)
	}

	args, err := decodeShareArgs(job)
	if err != nil {
		return nil, err
	}
	if args.TenantID != tenantID {
		// Reported as not-found rather than forbidden. Confirming the job
		// exists would tell a caller how many shares other tenants have run,
		// which is what sequential ids make cheap to probe for.
		return nil, ErrJobNotFound
	}

	return &models.ShareStatus{
		JobID: job.ID,
		State: string(job.State),
		// Success is the state being completed, never a string match on
		// anything else. kaufmann's per-VIN "Success" convention does not
		// apply to single-job statuses and mixing them is a recorded trap.
		IsSuccessful: job.State == rivertype.JobStateCompleted,
		Errors:       jobErrors(job),
	}, nil
}

// ErrJobNotFound covers both a job that does not exist and one belonging to
// another tenant — the caller cannot tell the two apart, which is deliberate.
var ErrJobNotFound = errors.New("share job not found")

func decodeShareArgs(job *rivertype.JobRow) (ShareArgs, error) {
	var args ShareArgs
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return args, fmt.Errorf("decode share job %d: %w", job.ID, err)
	}
	return args, nil
}

// jobErrors flattens River's attempt errors into plain strings.
//
// These reach the customer, so they are the worker's error text — which is
// written for that audience: "owner account has not authorized this tenant's
// signer" rather than a stack trace.
func jobErrors(job *rivertype.JobRow) []string {
	out := make([]string, 0, len(job.Errors))
	for _, e := range job.Errors {
		out = append(out, e.Error)
	}
	return out
}

// riverJobKind exists to keep the compiler honest: if ShareArgs.Kind ever
// changes, the queue and the status reader must move together.
var _ river.JobArgs = ShareArgs{}
