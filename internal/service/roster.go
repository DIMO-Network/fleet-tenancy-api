package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/lib/pq"
	"github.com/rs/zerolog"
)

// RosterService keeps the `vehicles` table — what a vehicle is and who owns it
// — reconciled against identity-api.
//
// Plan 07 step 3. This exists because three production vehicles had
// kaufmann_oracle.vins.owner contradicting the chain since a transfer, with
// nothing that would ever have noticed. Every writer of that column is one of
// kaufmann's own paths, so a transfer it did not perform is permanent
// divergence.
//
// THE RECONCILIATION IS THE FEATURE, not the table. A roster that were written
// once and then maintained by whoever performed a change would reproduce the
// bug it replaces, in a new schema. Owner is therefore re-read on every run and
// compared, never merged.
type RosterService struct {
	logger   *zerolog.Logger
	pdb      *db.Store
	identity gateway.IdentityAPI
}

func NewRosterService(logger *zerolog.Logger, pdb *db.Store, identity gateway.IdentityAPI) *RosterService {
	return &RosterService{logger: logger, pdb: pdb, identity: identity}
}

// OwnerChange is one vehicle whose on-chain owner no longer matches the roster.
//
// Reported as well as applied. Without this the job would silently correct the
// three known-wrong owners and leave nothing to show it had happened, so the
// next unexplained transfer would be exactly as invisible as this one was.
type OwnerChange struct {
	TokenID  int64
	Previous string
	New      string
}

// ReconcileReport is what one sweep saw. Returned rather than only logged so
// the command can set its exit status on it and a test can assert on it.
type ReconcileReport struct {
	// LicensesSwept is how many distinct client ids were asked.
	LicensesSwept int
	// LicensesFailed names client ids identity-api could not answer for. A
	// partial sweep must never be reported as a clean one: the vehicles behind
	// a failed licence are indistinguishable from vehicles nobody can see any
	// more, and treating them as the latter would mark a healthy fleet unseen.
	LicensesFailed []string

	VehiclesSeen int
	Inserted     int
	Updated      int
	OwnerChanges []OwnerChange
	MarkedUnseen int
	FirstRun     bool

	// EntitledFilled counts vehicles the licence sweep could not enumerate but
	// that an active entitlement named, fetched individually. Normally zero: a
	// customer's vehicles are usually shared with the licence serving them. A
	// non-zero count is worth understanding rather than ignoring — it means
	// somebody is entitled to a vehicle whose SACD we cannot read.
	EntitledFilled int
}

// Reconcile sweeps every developer licence this service holds, reads each
// licence's privileged vehicles from identity-api, and brings the roster into
// line with what the chain says.
//
// dryRun computes the whole report and writes nothing, so the first production
// run can be read by a human before it changes anything — which is what plan
// 07 step 3 asks for.
func (s *RosterService) Reconcile(ctx context.Context, dryRun bool) (*ReconcileReport, error) {
	report := &ReconcileReport{}

	var existingCount int
	if err := s.pdb.DBS().Reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM vehicles`).Scan(&existingCount); err != nil {
		return nil, fmt.Errorf("count roster: %w", err)
	}
	report.FirstRun = existingCount == 0

	clientIDs, err := s.licenceClientIDs(ctx)
	if err != nil {
		return nil, err
	}
	report.LicensesSwept = len(clientIDs)

	// One vehicle may be privileged to several licences — an operator's and its
	// customer's. Collapsing here rather than upserting twice keeps the seen
	// count honest and the owner comparison single-shot.
	byToken := make(map[int64]gateway.RosterVehicle)
	for _, clientID := range clientIDs {
		vehicles, verr := s.identity.PrivilegedVehicles(clientID)
		if verr != nil {
			// Carry on to the remaining licences — one bad licence should not
			// cost the rest their refresh — but the run is degraded, and
			// unseen-marking is suppressed below because of it.
			s.logger.Err(verr).Str("client_id", clientID).Msg("privileged vehicles, skipping licence")
			report.LicensesFailed = append(report.LicensesFailed, clientID)
			continue
		}
		for _, v := range vehicles {
			byToken[v.TokenID] = v
		}
	}
	// An entitled vehicle whose SACD is not shared with any licence we hold is
	// invisible to the sweep above, but its token id is this service's OWN
	// record — so it is knowable, and it must be in the roster. Once readers
	// cut over (step 4), an entitled vehicle missing here IS the empty-fleet
	// incident again, one layer down. Filled one at a time because there is
	// nothing to enumerate: identity-api answers for a token by id without
	// privilege, which is what makes this possible at all.
	filled, err := s.fillEntitledGaps(ctx, byToken)
	if err != nil {
		return nil, err
	}
	report.EntitledFilled = filled
	report.VehiclesSeen = len(byToken)

	tokens := make([]int64, 0, len(byToken))
	for id := range byToken {
		tokens = append(tokens, id)
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i] < tokens[j] })

	current, err := s.currentOwners(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	for _, id := range tokens {
		v := byToken[id]
		prev, known := current[id]
		switch {
		case !known:
			report.Inserted++
			// A first observation is not a transfer from nobody, so it is not
			// an owner change — but it is still worth a row in the change log,
			// with a NULL previous owner, so the history starts where we
			// started looking rather than at the first later transfer.
		case prev != v.Owner && v.Owner != "":
			report.Updated++
			report.OwnerChanges = append(report.OwnerChanges, OwnerChange{
				TokenID: id, Previous: prev, New: v.Owner,
			})
		default:
			report.Updated++
		}

		if dryRun {
			continue
		}
		if err := s.upsert(ctx, v, now, !known, prev); err != nil {
			return nil, err
		}
	}

	// Vehicles no licence returned. Only meaningful when every licence
	// answered: after a partial sweep the missing ones are missing because a
	// call failed, and marking them would record an outage as a fleet change.
	if len(report.LicensesFailed) == 0 {
		n, uerr := s.markUnseen(ctx, tokens, now, dryRun)
		if uerr != nil {
			return nil, uerr
		}
		report.MarkedUnseen = n
	}

	return report, nil
}

// fillEntitledGaps adds vehicles named by an active entitlement that the
// licence sweep did not return, fetching each by token id.
//
// A missing one is reported and skipped, never fatal: an entitlement pointing
// at a token identity-api does not know is a data problem to surface, not a
// reason to abandon the whole reconcile.
func (s *RosterService) fillEntitledGaps(ctx context.Context, byToken map[int64]gateway.RosterVehicle) (int, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT DISTINCT vehicle_token_id FROM vehicle_entitlements WHERE revoked_at IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("list entitled token ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var missing []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan entitled token id: %w", err)
		}
		if _, ok := byToken[id]; !ok {
			missing = append(missing, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })

	filled := 0
	for _, id := range missing {
		v, verr := s.identity.VehicleDetail(id)
		if verr != nil {
			s.logger.Warn().Err(verr).Int64("vehicle_token_id", id).
				Msg("entitled vehicle not resolvable from identity-api; roster will not hold it")
			continue
		}
		byToken[id] = *v
		filled++
	}
	if filled > 0 {
		s.logger.Info().Int("count", filled).
			Msg("entitled vehicles filled individually — not privileged to any licence we hold")
	}
	return filled, nil
}

// licenceClientIDs lists every developer-licence client id this service holds.
//
// This is the roster's population: the union of every tenant's privileged set
// is every vehicle anybody on this platform can see. Deliberately NOT
// vehicle_entitlements — those cover explicit-mode tenants only, and would
// leave out the 178 vehicles belonging to self-serve tenants' own licences.
// A roster with a known hole is what disqualified kaufmann from holding it.
func (s *RosterService) licenceClientIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT DISTINCT dimo_client_id FROM tenant_credentials
		  WHERE dimo_client_id IS NOT NULL AND dimo_client_id <> ''
		  ORDER BY dimo_client_id`)
	if err != nil {
		return nil, fmt.Errorf("list licence client ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan client id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// currentOwners is the roster's present answer, for comparison.
func (s *RosterService) currentOwners(ctx context.Context) (map[int64]string, error) {
	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT vehicle_token_id, COALESCE(owner, '') FROM vehicles`)
	if err != nil {
		return nil, fmt.Errorf("read roster owners: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make(map[int64]string)
	for rows.Next() {
		var (
			id    int64
			owner string
		)
		if err := rows.Scan(&id, &owner); err != nil {
			return nil, fmt.Errorf("scan roster owner: %w", err)
		}
		out[id] = owner
	}
	return out, rows.Err()
}

// upsert writes one vehicle's chain-derived fields.
//
// VIN and PLATE ARE FILLED FORWARD, NEVER CLEARED. identity-api does not serve
// them; kaufmann writes plates from the Chilean registry, which makes this
// table a consumer for that field rather than its source. An upsert that wrote
// the struct wholesale would blank a known plate on every run — the roster
// would be authoritative for a field it never reads, which is the failure this
// whole plan is about, pointed the other way.
func (s *RosterService) upsert(ctx context.Context, v gateway.RosterVehicle, now time.Time, isNew bool, prevOwner string) error {
	_, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`INSERT INTO vehicles (vehicle_token_id, owner, definition_id, make, model, year,
		                       minted_at, synthetic_device_token_id, aftermarket_device_token_id,
		                       first_seen_at, reconciled_at, unseen_since, updated_at)
		 VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''),
		         NULLIF($6, 0), $7, $9, $10, $8, $8, NULL, $8)
		 ON CONFLICT (vehicle_token_id) DO UPDATE SET
		     owner         = COALESCE(NULLIF(EXCLUDED.owner, ''), vehicles.owner),
		     definition_id = COALESCE(NULLIF(EXCLUDED.definition_id, ''), vehicles.definition_id),
		     make          = COALESCE(NULLIF(EXCLUDED.make, ''), vehicles.make),
		     model         = COALESCE(NULLIF(EXCLUDED.model, ''), vehicles.model),
		     year          = COALESCE(EXCLUDED.year, vehicles.year),
		     minted_at     = COALESCE(EXCLUDED.minted_at, vehicles.minted_at),
		     -- Device ids are OVERWRITTEN, including to NULL, where every
		     -- column above is filled forward. The difference is who the source
		     -- is: identity-api serves these and does not serve VIN or plate, so
		     -- a NULL here is the chain saying "nothing is paired" rather than
		     -- "we did not read it". Filling them forward would leave a vehicle
		     -- reading as connected forever after its device was unpaired.
		     synthetic_device_token_id   = EXCLUDED.synthetic_device_token_id,
		     aftermarket_device_token_id = EXCLUDED.aftermarket_device_token_id,
		     reconciled_at = EXCLUDED.reconciled_at,
		     -- Seen again, so it is no longer unseen. Set unconditionally: a
		     -- vehicle that reappears after an SACD was restored must clear
		     -- this, or the column records the first loss forever.
		     unseen_since  = NULL,
		     updated_at    = EXCLUDED.updated_at`,
		v.TokenID, v.Owner, v.DefinitionID, v.Make, v.Model, v.Year, v.MintedAt, now,
		v.SyntheticDeviceTokenID, v.AftermarketDeviceTokenID)
	if err != nil {
		return fmt.Errorf("upsert vehicle %d: %w", v.TokenID, err)
	}

	if v.Owner == "" || (!isNew && prevOwner == v.Owner) {
		return nil
	}
	var previous any
	if !isNew {
		previous = prevOwner
	}
	if _, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`INSERT INTO vehicle_owner_changes (vehicle_token_id, previous_owner, new_owner, observed_at)
		 VALUES ($1, $2, $3, $4)`,
		v.TokenID, previous, v.Owner, now); err != nil {
		return fmt.Errorf("record owner change for %d: %w", v.TokenID, err)
	}
	return nil
}

// markUnseen stamps rows no licence returned this run.
//
// Stamped, never deleted. A vehicle dropping out of every privileged set
// usually means an SACD was revoked or a licence rotated, not that the vehicle
// ceased to exist, and deleting would discard the only record that we ever knew
// it. Rows already stamped keep their original timestamp — the question this
// column answers is "since when", not "as of when did we last check".
func (s *RosterService) markUnseen(ctx context.Context, seen []int64, now time.Time, dryRun bool) (int, error) {
	if dryRun {
		var n int
		err := s.pdb.DBS().Reader.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM vehicles
			  WHERE unseen_since IS NULL AND NOT (vehicle_token_id = ANY($1))`,
			pq.Array(seen)).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count unseen: %w", err)
		}
		return n, nil
	}

	res, err := s.pdb.DBS().Writer.ExecContext(ctx,
		`UPDATE vehicles SET unseen_since = $2, updated_at = $2
		  WHERE unseen_since IS NULL AND NOT (vehicle_token_id = ANY($1))`,
		pq.Array(seen), now)
	if err != nil {
		return 0, fmt.Errorf("mark unseen: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
