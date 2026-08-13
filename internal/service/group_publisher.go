package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/DIMO-Network/cloudevent"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog"
	"github.com/segmentio/ksuid"
)

// VehicleGroupsCloudEventType is the registered Attest API type for a
// vehicle's group-membership document. Unchanged from both source apps — the
// wire contract survives the move.
const VehicleGroupsCloudEventType = "dimo.document.vehicle.groups"

// GroupAttestationProducer identifies this service in the CE producer field.
// It is deliberately distinct from both retired producers ("fleet-lite-app"
// and kaufmann's did:ethr per-tenant value): fleet-lite's importer unioned
// per producer, and a colliding value would have silently overwritten one
// side's contribution during the rollout window.
const GroupAttestationProducer = "fleet-tenancy-api"

// groupAttestationRef is one group inside the published document. Field order
// is the wire contract — the signature covers these marshaled bytes.
type groupAttestationRef struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

type vehicleGroupsDocument struct {
	Groups []groupAttestationRef `json:"groups"`
}

// GroupPublisher publishes dimo.document.vehicle.groups CloudEvents for every
// vehicle whose current group set no longer matches what was last published —
// P4's single publisher, replacing one publisher in each source app.
//
// It is scan-based, not enqueued per write, on purpose: kaufmann's
// enqueue-based publisher once coalesced a rename against completed jobs and
// silently published nothing (kaufmann-oracle#192). A scan against current
// state has nothing to coalesce; whatever is true at scan time is what gets
// published, and a missed run is caught by the next one.
//
// It runs as its own deployable (a CronJob), never inside the API process —
// per-vehicle publishing is heavy background work and must not share the fate
// of /v1/authz (the plan's R5 risk note).
type GroupPublisher struct {
	logger      *zerolog.Logger
	pdb         *db.Store
	settings    *config.Settings
	credentials *CredentialService
	attest      gateway.AttestAPI
}

func NewGroupPublisher(logger *zerolog.Logger, pdb *db.Store, settings *config.Settings,
	credentials *CredentialService, attest gateway.AttestAPI) *GroupPublisher {
	return &GroupPublisher{logger: logger, pdb: pdb, settings: settings,
		credentials: credentials, attest: attest}
}

// PublishResult is one run's accounting. Failed > 0 means the run must exit
// non-zero — a partial publish is not a success, because each unpublished
// vehicle is one whose outward record is stale.
//
// The split between what the scan PLANNED and what the wire ACCEPTED is not
// cosmetic. An earlier version had planPublishes and the publish loop both
// incrementing Published, which reported exactly double on a clean run and,
// worse, would have inflated the success count on a run that partly failed —
// 300 published against 57 failed printing as 657. The membership backfill
// hit the same class of bug (counters reporting rows processed rather than
// rows written). So: the plan owns Checked/Unchanged/Planned, the publish
// loop owns Published/Retracted/Failed/SkippedVehicles, and the two are
// reconciled by Balances below rather than trusted.
type PublishResult struct {
	Checked int // vehicles whose current digest was compared against the recorded one
	Planned int // vehicles the scan selected to publish

	Published       int // non-empty documents the wire accepted
	Retracted       int // empty-set documents the wire accepted
	Failed          int
	SkippedVehicles int // vehicles under a tenant with no usable credential

	Unchanged      int
	SkippedTenants int // tenants with no usable credential — their vehicles are unpublishable
}

// Balances reports whether every planned vehicle is accounted for by exactly
// one outcome. A false here means the accounting is lying, which is worth
// surfacing even though it does not change what was published.
func (r *PublishResult) Balances() bool {
	return r.Planned == r.Published+r.Retracted+r.Failed+r.SkippedVehicles
}

// vehKey identifies one vehicle within one tenant.
type vehKey struct {
	tenantID string
	tokenID  int64
}

// pendingVehicle is one vehicle whose attestation is out of date.
type pendingVehicle struct {
	tenantID string
	tokenID  int64
	groups   []groupAttestationRef
	dataJSON []byte
	digest   string
}

// Run scans and publishes. dryRun reports what would be published without
// touching the wire or the state table.
func (p *GroupPublisher) Run(ctx context.Context, tenantFilter string, dryRun bool) (*PublishResult, error) {
	current, err := p.loadCurrentGroups(ctx, tenantFilter)
	if err != nil {
		return nil, err
	}
	published, err := p.loadPublishedDigests(ctx, tenantFilter)
	if err != nil {
		return nil, err
	}

	pending, res, err := planPublishes(current, published)
	if err != nil {
		return nil, err
	}

	if dryRun {
		// Planned is what a dry run is for, so it is reported as-is. Published
		// and Retracted stay zero because nothing reached the wire — a dry run
		// that reported them would be claiming an outcome it did not have.
		for _, v := range pending {
			p.logger.Info().Str("tenant_id", v.tenantID).Int64("token_id", v.tokenID).
				Int("groups", len(v.groups)).Bool("dry_run", true).Msg("would publish vehicle groups attestation")
		}
		return res, nil
	}

	// Publish tenant by tenant so credential resolution happens once per
	// tenant and a tenant with no usable credential is skipped as a unit,
	// loudly, exactly as kaufmann's worker treated unconfigured tenants.
	byTenant := map[string][]pendingVehicle{}
	for _, v := range pending {
		byTenant[v.tenantID] = append(byTenant[v.tenantID], v)
	}
	tenantIDs := make([]string, 0, len(byTenant))
	for id := range byTenant {
		tenantIDs = append(tenantIDs, id)
	}
	sort.Strings(tenantIDs)

	for _, tenantID := range tenantIDs {
		vehicles := byTenant[tenantID]
		token, err := p.credentials.DeveloperJWT(ctx, tenantID)
		if err != nil {
			if errors.Is(err, ErrNoCredential) {
				p.logger.Warn().Str("tenant_id", tenantID).Int("vehicles", len(vehicles)).
					Msg("tenant has no usable credential — its group attestations cannot be published")
				res.SkippedTenants++
				res.SkippedVehicles += len(vehicles)
				continue
			}
			p.logger.Err(err).Str("tenant_id", tenantID).Int("vehicles", len(vehicles)).
				Msg("developer JWT for publisher")
			res.Failed += len(vehicles)
			continue
		}

		for _, v := range vehicles {
			if err := p.publishOne(ctx, token.Token, v); err != nil {
				p.logger.Err(err).Str("tenant_id", v.tenantID).Int64("token_id", v.tokenID).
					Msg("publish vehicle groups attestation")
				res.Failed++
				continue
			}
			if len(v.groups) == 0 {
				res.Retracted++
			} else {
				res.Published++
			}
		}
	}
	return res, nil
}

// planPublishes computes which vehicles need publishing. Pure, so the
// digest/retraction rules are testable without a database.
//
// A vehicle appears in the plan when its current document digest differs from
// the recorded one. A vehicle with no groups and no state row is not a
// retraction — nothing was ever asserted for it — and never publishes.
func planPublishes(current map[vehKey][]groupAttestationRef, published map[vehKey]string) ([]pendingVehicle, *PublishResult, error) {
	keys := map[vehKey]bool{}
	for k := range current {
		keys[k] = true
	}
	for k := range published {
		keys[k] = true
	}
	ordered := make([]vehKey, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].tenantID != ordered[j].tenantID {
			return ordered[i].tenantID < ordered[j].tenantID
		}
		return ordered[i].tokenID < ordered[j].tokenID
	})

	res := &PublishResult{}
	var pending []pendingVehicle
	for _, k := range ordered {
		groups := current[k]
		_, everPublished := published[k]
		if len(groups) == 0 && !everPublished {
			continue
		}
		res.Checked++

		if groups == nil {
			groups = []groupAttestationRef{}
		}
		data, err := json.Marshal(vehicleGroupsDocument{Groups: groups})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal groups document: %w", err)
		}
		digest := sha256Hex(data)
		if published[k] == digest {
			res.Unchanged++
			continue
		}

		pending = append(pending, pendingVehicle{
			tenantID: k.tenantID, tokenID: k.tokenID,
			groups: groups, dataJSON: data, digest: digest,
		})
		res.Planned++
	}
	return pending, res, nil
}

// publishOne signs, delivers and records one vehicle's document. The state row
// is written only after the wire accepted the event — a delivery failure must
// leave the vehicle pending for the next run, never marked done.
func (p *GroupPublisher) publishOne(ctx context.Context, developerJWT string, v pendingVehicle) error {
	sig, cred, err := p.credentials.SignAsTenant(ctx, v.tenantID, v.dataJSON)
	if err != nil {
		return err
	}

	subject := cloudevent.ERC721DID{
		ChainID:         uint64(p.settings.ChainID),
		ContractAddress: common.HexToAddress(p.settings.VehicleNftAddress),
		TokenID:         big.NewInt(v.tokenID),
	}.String()

	event := cloudevent.CloudEvent[json.RawMessage]{
		CloudEventHeader: cloudevent.CloudEventHeader{
			ID:              ksuid.New().String(),
			Source:          common.HexToAddress(cred.ClientID).Hex(),
			Producer:        GroupAttestationProducer,
			SpecVersion:     cloudevent.SpecVersion,
			Subject:         subject,
			Time:            time.Now().UTC(),
			Type:            VehicleGroupsCloudEventType,
			DataContentType: "application/json",
			Signature:       sig,
			Tags:            []string{"vehicle.groups", "vehicle"},
		},
		Data: v.dataJSON,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal cloud event: %w", err)
	}

	if err := p.attest.SubmitCloudEvent(ctx, developerJWT, payload); err != nil {
		return err
	}

	if _, err := p.pdb.DBS().Writer.ExecContext(ctx, `
		INSERT INTO vehicle_group_attestation_state (tenant_id, vehicle_token_id, published_digest, published_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (tenant_id, vehicle_token_id) DO UPDATE SET
			published_digest = EXCLUDED.published_digest,
			published_at = NOW(),
			updated_at = NOW()`,
		v.tenantID, v.tokenID, v.digest); err != nil {
		// The event is on the wire but the bookkeeping failed: the next run
		// republishes the same content, which consumers treat as a no-op.
		return fmt.Errorf("record published digest: %w", err)
	}

	p.logger.Info().Str("tenant_id", v.tenantID).Int64("token_id", v.tokenID).
		Int("groups", len(v.groups)).Msg("published vehicle groups attestation")
	return nil
}

// loadCurrentGroups returns every grouped vehicle's document content, groups
// name-ordered — the ordering both source apps published, kept so unchanged
// memberships hash to unchanged digests across the handover.
func (p *GroupPublisher) loadCurrentGroups(ctx context.Context, tenantFilter string) (map[vehKey][]groupAttestationRef, error) {
	rows, err := p.pdb.DBS().Reader.QueryContext(ctx, `
		SELECT v.tenant_id, v.vehicle_token_id, fg.id, fg.name, fg.color
		  FROM vehicle_fleet_groups v
		  JOIN fleet_groups fg ON fg.id = v.fleet_group_id AND fg.tenant_id = v.tenant_id
		 WHERE ($1 = '' OR v.tenant_id::text = $1)
		 ORDER BY v.tenant_id, v.vehicle_token_id, fg.name, fg.id`, tenantFilter)
	if err != nil {
		return nil, fmt.Errorf("load current groups: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := map[vehKey][]groupAttestationRef{}
	for rows.Next() {
		var k vehKey
		var ref groupAttestationRef
		if err := rows.Scan(&k.tenantID, &k.tokenID, &ref.ID, &ref.Name, &ref.Color); err != nil {
			return nil, fmt.Errorf("scan current groups: %w", err)
		}
		out[k] = append(out[k], ref)
	}
	return out, rows.Err()
}

func (p *GroupPublisher) loadPublishedDigests(ctx context.Context, tenantFilter string) (map[vehKey]string, error) {
	rows, err := p.pdb.DBS().Reader.QueryContext(ctx, `
		SELECT tenant_id, vehicle_token_id, published_digest
		  FROM vehicle_group_attestation_state
		 WHERE ($1 = '' OR tenant_id::text = $1)`, tenantFilter)
	if err != nil {
		return nil, fmt.Errorf("load published digests: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := map[vehKey]string{}
	for rows.Next() {
		var k vehKey
		var digest string
		if err := rows.Scan(&k.tenantID, &k.tokenID, &digest); err != nil {
			return nil, fmt.Errorf("scan published digest: %w", err)
		}
		out[k] = digest
	}
	return out, rows.Err()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
