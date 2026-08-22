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

// EnqueueRevoke queues a revocation of a share and returns its job id.
func (q *Queue) EnqueueRevoke(ctx context.Context, args RevokeArgs) (int64, error) {
	if q == nil {
		return 0, ErrQueueUnavailable
	}
	res, err := q.Client.Insert(ctx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("enqueue revoke: %w", err)
	}
	return res.Job.ID, nil
}

// Status reports on a queued share OR its revocation.
//
// Scoped by tenant: the job id alone would otherwise let any caller read any
// tenant's share outcome, and job ids are sequential integers. The tenant is
// compared against the one stored in the job's own arguments, so the check
// cannot drift from what the job will actually do.
//
// BOTH KINDS ARE ANSWERED HERE, which is a deliberate exception to the kind
// check below rather than an oversight. Granting and revoking are two
// directions of one relationship, they carry the same identifying fields, and a
// caller that polls a share should poll its revocation the same way rather than
// learning a second protocol for it. What the kind check keeps out is
// shared-operation jobs, which are a different surface with a different
// contract — see SharedOpStatus.
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
	if job.Kind != (ShareArgs{}).Kind() && job.Kind != (RevokeArgs{}).Kind() {
		// The queue also carries shared-operation jobs, whose args decode into
		// ShareArgs cleanly enough (tenantId overlaps) that without this check
		// a shared-op job would be reported here as a share. Vacuous while the
		// queue held one kind; load-bearing since it holds three.
		return nil, ErrJobNotFound
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

// EnqueueSharedOp queues a typed shared-account operation and returns its job
// id. Same queue as shares: the workers are bounded together deliberately,
// because everything here is dominated by waiting on a bundler and this
// process's first duty is /v1/authz.
func (q *Queue) EnqueueSharedOp(ctx context.Context, args SharedOpArgs) (int64, error) {
	if q == nil {
		return 0, ErrQueueUnavailable
	}
	res, err := q.Client.Insert(ctx, args, nil)
	if err != nil {
		return 0, fmt.Errorf("enqueue shared operation: %w", err)
	}
	return res.Job.ID, nil
}

// SharedOpStatus reports on a queued shared operation, in exactly the shape
// Status reports a share — the plan's requirement is that the two mirror.
//
// Tenant-scoped for the same reason as Status, and additionally scoped to the
// shared-operation job kind: the two surfaces draw from one River queue with
// one id sequence, and answering this endpoint for a share job would let the
// two protocols blur into each other. A share job id asked about here is
// reported not-found, exactly like another tenant's job.
func (q *Queue) SharedOpStatus(ctx context.Context, tenantID string, jobID int64) (*models.ShareStatus, error) {
	if q == nil {
		return nil, ErrQueueUnavailable
	}
	job, err := q.Client.JobGet(ctx, jobID)
	if err != nil {
		if errors.Is(err, rivertype.ErrNotFound) {
			return nil, ErrJobNotFound
		}
		return nil, fmt.Errorf("read shared-op job %d: %w", jobID, err)
	}
	if job.Kind != (SharedOpArgs{}).Kind() {
		return nil, ErrJobNotFound
	}

	var args SharedOpArgs
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return nil, fmt.Errorf("decode shared-op job %d: %w", job.ID, err)
	}
	if args.TenantID != tenantID {
		// Not-found rather than forbidden, exactly as in Status: confirming
		// the job exists would tell a caller what other tenants have run.
		return nil, ErrJobNotFound
	}

	return &models.ShareStatus{
		JobID:        job.ID,
		State:        string(job.State),
		IsSuccessful: job.State == rivertype.JobStateCompleted,
		Errors:       jobErrors(job),
	}, nil
}

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
