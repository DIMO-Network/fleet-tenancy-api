package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/shared/pkg/db"
	"github.com/google/subcommands"
	"github.com/rs/zerolog"
)

// backfillGroupsCmd copies fleet groups and their vehicle memberships from both
// source systems into this service — P3 of docs/plans/01-groups-into-tenancy.md.
//
// Unlike the tenant backfill there is no id migration and no re-encryption:
// ids are already <tenant-uuid>_<slug> in both sources after R1, tenant uuids
// are already unified, and groups hold no secrets. The work here is the merge.
//
// THE MERGE, and why each rule is what it is:
//
//   - A group can exist in BOTH sources — the Kaufmann tenant lives in both
//     systems and the attestation sync keeps two copies of its groups. Ids
//     match by construction, so the overlap is detected exactly.
//
//   - Metadata (name, color) comes from whichever side was updated more
//     recently. This is the same rule fleet-lite's importer had to learn the
//     hard way (fleet-lite-app#111): adopting metadata without comparing
//     timestamps made the surviving name depend on processing order and once
//     rewrote a group's name 40 times in one import.
//
//   - Memberships union. The sources converge through the attestation stream,
//     so disagreements are sync lag, not intent; dropping the laggard's rows
//     would re-create the data loss this move exists to end.
//
//   - The write REPLACES this service's group tables wholesale. Until P4 the
//     sources stay authoritative and nothing else writes groups here, so a
//     re-run must converge on what the sources currently say — an upsert that
//     never deletes would accumulate rows the sources have since removed. The
//     same reasoning made the member backfill merge in memory, not ON CONFLICT.
//
// Nothing is written unless every check passes: every referenced tenant exists
// here, no two groups in one tenant share a name (the UNIQUE would abort the
// transaction mid-write with a far worse error), and no membership points
// across tenants.
type backfillGroupsCmd struct {
	logger   zerolog.Logger
	settings config.Settings

	dryRun bool
}

func (*backfillGroupsCmd) Name() string { return "backfill-groups" }
func (*backfillGroupsCmd) Synopsis() string {
	return "copy fleet groups and vehicle memberships from the source systems"
}
func (*backfillGroupsCmd) Usage() string {
	return `backfill-groups [-dry-run]:
	Copies fleet_groups and their vehicle memberships from kaufmann-oracle and
	fleet-lite-app into this service (P3 of the groups move).

	Connection details come from the environment, matching the backfill command:

	  BACKFILL_KAUFMANN_DSN    postgres://... (kaufmann_oracle)
	  BACKFILL_FLEETLITE_DSN   postgres://...?search_path=fleets_lite

	No encryption keys are needed — groups hold no secrets.

	The write replaces this service's group tables wholesale, so a re-run
	converges on whatever the sources currently say. Run it again after any
	local group write in either app until P4 cuts writes over.

	-dry-run reports the merged set, every metadata disagreement between the
	sources, and any dangling scope_group_ids / source_group_id references,
	writing nothing.
  `
}

func (p *backfillGroupsCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&p.dryRun, "dry-run", false, "report only; write nothing")
}

// srcGroup is one group row as a source system holds it.
type srcGroup struct {
	id        string
	tenantID  string
	name      string
	color     string
	createdAt time.Time
	updatedAt time.Time
	source    string
}

// groupMembership is one (tenant, vehicle, group) row.
type groupMembership struct {
	tenantID  string
	tokenID   int64
	groupID   string
	createdAt time.Time
}

type membKey struct {
	tenantID string
	tokenID  int64
	groupID  string
}

// mergedGroup is one group folded across every source that holds it.
type mergedGroup struct {
	srcGroup
	sources int
}

// mergeGroups folds both sources' group rows into one set keyed by id.
// Metadata is adopted from the side with the newer updated_at; created_at
// keeps the earlier value so the row's history stays honest. A same-id pair
// whose tenant uuids disagree is impossible by construction (the id embeds
// the tenant uuid) — it is checked anyway, because "impossible" rows are
// exactly what a migration must refuse to guess about.
func mergeGroups(groups []srcGroup) (map[string]*mergedGroup, error) {
	merged := map[string]*mergedGroup{}
	for _, g := range groups {
		cur, ok := merged[g.id]
		if !ok {
			merged[g.id] = &mergedGroup{srcGroup: g, sources: 1}
			continue
		}
		if cur.tenantID != g.tenantID {
			return nil, fmt.Errorf("group %s claims tenant %s in %s but %s in %s",
				g.id, cur.tenantID, cur.source, g.tenantID, g.source)
		}
		cur.sources++
		if g.updatedAt.After(cur.updatedAt) {
			cur.name, cur.color, cur.updatedAt, cur.source = g.name, g.color, g.updatedAt, g.source
		}
		if g.createdAt.Before(cur.createdAt) {
			cur.createdAt = g.createdAt
		}
	}
	return merged, nil
}

// checkNameCollisions finds two distinct group ids in one tenant sharing an
// exact name. The target schema's UNIQUE (tenant_id, name) would refuse the
// second insert mid-transaction; refusing up front names both ids so someone
// can rename one at the source instead of decoding a constraint error.
func checkNameCollisions(merged map[string]*mergedGroup) error {
	byTenantName := map[string]string{}
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		g := merged[id]
		k := g.tenantID + "\x00" + g.name
		if other, dup := byTenantName[k]; dup {
			return fmt.Errorf("groups %s and %s in tenant %s both carry the name %q — rename one at the source first",
				other, id, g.tenantID, g.name)
		}
		byTenantName[k] = id
	}
	return nil
}

// mergeMemberships unions both sources' membership rows, keeping the earliest
// created_at, and refuses any row whose group is unknown or whose tenant does
// not match the group's. fleet-lite's table carries its own tenant_id column
// with no composite FK to the group, so a mismatched row is representable
// there and must not be copied into a schema whose whole point is that it
// is not.
func mergeMemberships(rows []groupMembership, merged map[string]*mergedGroup) (map[membKey]time.Time, error) {
	out := map[membKey]time.Time{}
	for _, m := range rows {
		g, ok := merged[m.groupID]
		if !ok {
			return nil, fmt.Errorf("membership (tenant %s, vehicle %d) references unknown group %s",
				m.tenantID, m.tokenID, m.groupID)
		}
		if g.tenantID != m.tenantID {
			return nil, fmt.Errorf("membership of vehicle %d claims tenant %s but group %s belongs to tenant %s",
				m.tokenID, m.tenantID, m.groupID, g.tenantID)
		}
		k := membKey{tenantID: m.tenantID, tokenID: m.tokenID, groupID: m.groupID}
		if cur, dup := out[k]; !dup || m.createdAt.Before(cur) {
			out[k] = m.createdAt
		}
	}
	return out, nil
}

func (p *backfillGroupsCmd) Execute(ctx context.Context, _ *flag.FlagSet, _ ...interface{}) subcommands.ExitStatus {
	kDSN, fDSN := os.Getenv("BACKFILL_KAUFMANN_DSN"), os.Getenv("BACKFILL_FLEETLITE_DSN")
	if kDSN == "" || fDSN == "" {
		p.logger.Error().Msg("BACKFILL_KAUFMANN_DSN and BACKFILL_FLEETLITE_DSN are required")
		return subcommands.ExitUsageError
	}

	kdb, err := sql.Open("postgres", kDSN)
	if err != nil {
		p.logger.Err(err).Msg("open kaufmann")
		return subcommands.ExitFailure
	}
	defer func() { _ = kdb.Close() }()
	fdb, err := sql.Open("postgres", fDSN)
	if err != nil {
		p.logger.Err(err).Msg("open fleet-lite")
		return subcommands.ExitFailure
	}
	defer func() { _ = fdb.Close() }()

	store := db.NewDbConnectionFromSettings(ctx, &p.settings.DB, true)
	store.WaitForDB(p.logger)
	target := store.DBS().Writer.DB

	// ---- Pass 1: read and verify everything before writing anything ----

	kGroups, err := readGroups(ctx, kdb, `
		SELECT id, tenant_id, name, color, created_at, updated_at
		  FROM kaufmann_oracle.fleet_groups`, "kaufmann")
	if err != nil {
		p.logger.Err(err).Msg("read kaufmann groups")
		return subcommands.ExitFailure
	}
	fGroups, err := readGroups(ctx, fdb, `
		SELECT id, tenant_id, name, color, created_at, updated_at
		  FROM fleet_groups`, "fleet-lite")
	if err != nil {
		p.logger.Err(err).Msg("read fleet-lite groups")
		return subcommands.ExitFailure
	}

	merged, err := mergeGroups(append(append([]srcGroup{}, kGroups...), fGroups...))
	if err != nil {
		p.logger.Err(err).Msg("merge groups — aborting, nothing written")
		return subcommands.ExitFailure
	}
	// Surface every metadata disagreement the newer-side rule resolved. These
	// are the pre-existing inconsistencies the move exists to end; they should
	// be seen, not silently absorbed.
	for _, id := range sortedGroupIDs(merged) {
		g := merged[id]
		if g.sources < 2 {
			continue
		}
		for _, src := range [][]srcGroup{kGroups, fGroups} {
			for _, o := range src {
				if o.id == id && (o.name != g.name || o.color != g.color) {
					p.logger.Warn().Str("group_id", id).
						Str("kept", fmt.Sprintf("%q %s (%s, updated %s)", g.name, g.color, g.source, g.updatedAt.Format(time.RFC3339))).
						Str("superseded", fmt.Sprintf("%q %s (%s, updated %s)", o.name, o.color, o.source, o.updatedAt.Format(time.RFC3339))).
						Msg("sources disagree on group metadata — newer side kept")
				}
			}
		}
	}
	if err := checkNameCollisions(merged); err != nil {
		p.logger.Err(err).Msg("name collision — aborting, nothing written")
		return subcommands.ExitFailure
	}

	kMembs, err := readMemberships(ctx, kdb, `
		SELECT fg.tenant_id, vfg.vehicle_token_id, vfg.fleet_group_id, vfg.created_at
		  FROM kaufmann_oracle.vin_fleet_groups vfg
		  JOIN kaufmann_oracle.fleet_groups fg ON fg.id = vfg.fleet_group_id`)
	if err != nil {
		p.logger.Err(err).Msg("read kaufmann memberships")
		return subcommands.ExitFailure
	}
	fMembs, err := readMemberships(ctx, fdb, `
		SELECT tenant_id, token_id, fleet_group_id, created_at
		  FROM vehicle_fleet_groups`)
	if err != nil {
		p.logger.Err(err).Msg("read fleet-lite memberships")
		return subcommands.ExitFailure
	}
	memberships, err := mergeMemberships(append(append([]groupMembership{}, kMembs...), fMembs...), merged)
	if err != nil {
		p.logger.Err(err).Msg("merge memberships — aborting, nothing written")
		return subcommands.ExitFailure
	}

	// Every tenant a group names must already exist here — a missing one means
	// the tenant backfill has not covered it, and FK errors mid-write are a
	// worse way to find that out.
	if err := p.checkTenantsExist(ctx, target, merged); err != nil {
		p.logger.Err(err).Msg("tenant check — aborting, nothing written")
		return subcommands.ExitFailure
	}

	overlapGroups := 0
	for _, g := range merged {
		if g.sources > 1 {
			overlapGroups++
		}
	}
	p.logger.Info().
		Int("kaufmann_groups", len(kGroups)).
		Int("fleetlite_groups", len(fGroups)).
		Int("merged_groups", len(merged)).
		Int("overlapping_groups", overlapGroups).
		Int("kaufmann_memberships", len(kMembs)).
		Int("fleetlite_memberships", len(fMembs)).
		Int("merged_memberships", len(memberships)).
		Int("overlapping_memberships", len(kMembs)+len(fMembs)-len(memberships)).
		Bool("dry_run", p.dryRun).
		Msg("verification complete")

	if p.dryRun {
		p.reportDanglingRefs(ctx, target, merged)
		return subcommands.ExitSuccess
	}

	// ---- Pass 2: write, one transaction, replace wholesale ----

	tx, err := target.BeginTx(ctx, nil)
	if err != nil {
		p.logger.Err(err).Msg("begin")
		return subcommands.ExitFailure
	}
	defer func() { _ = tx.Rollback() }()

	// vehicle_fleet_groups goes first only for tidiness — deleting the groups
	// would cascade it anyway.
	if _, err := tx.ExecContext(ctx, `DELETE FROM vehicle_fleet_groups`); err != nil {
		p.logger.Err(err).Msg("clear memberships")
		return subcommands.ExitFailure
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM fleet_groups`)
	if err != nil {
		p.logger.Err(err).Msg("clear groups")
		return subcommands.ExitFailure
	}
	if n, _ := res.RowsAffected(); n > 0 {
		p.logger.Info().Int64("replaced_groups", n).Msg("existing rows replaced — sources are authoritative until P4")
	}

	for _, id := range sortedGroupIDs(merged) {
		g := merged[id]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO fleet_groups (id, tenant_id, name, color, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			g.id, g.tenantID, g.name, g.color, g.createdAt, g.updatedAt); err != nil {
			p.logger.Err(err).Str("group_id", g.id).Msg("insert group")
			return subcommands.ExitFailure
		}
	}

	keys := make([]membKey, 0, len(memberships))
	for k := range memberships {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].groupID != keys[j].groupID {
			return keys[i].groupID < keys[j].groupID
		}
		return keys[i].tokenID < keys[j].tokenID
	})
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO vehicle_fleet_groups (tenant_id, vehicle_token_id, fleet_group_id, created_at)
			VALUES ($1, $2, $3, $4)`,
			k.tenantID, k.tokenID, k.groupID, memberships[k]); err != nil {
			p.logger.Err(err).Str("group_id", k.groupID).Int64("token_id", k.tokenID).Msg("insert membership")
			return subcommands.ExitFailure
		}
	}

	if err := tx.Commit(); err != nil {
		p.logger.Err(err).Msg("commit")
		return subcommands.ExitFailure
	}

	p.logger.Info().
		Int("groups", len(merged)).
		Int("memberships", len(memberships)).
		Msg("group backfill complete")

	p.reportDanglingRefs(ctx, target, merged)
	return subcommands.ExitSuccess
}

// checkTenantsExist verifies every tenant the merged groups reference has a
// row here.
func (p *backfillGroupsCmd) checkTenantsExist(ctx context.Context, target *sql.DB, merged map[string]*mergedGroup) error {
	want := map[string]bool{}
	for _, g := range merged {
		want[g.tenantID] = true
	}
	rows, err := target.QueryContext(ctx, `SELECT id FROM tenants`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		delete(want, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for id := range want {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return fmt.Errorf("groups reference %d tenant(s) with no row here — run backfill first: %v", len(missing), missing)
	}
	return nil
}

// reportDanglingRefs names every scope_group_ids / source_group_id value that
// matches no group in the merged set. Informational: these columns predate the
// groups' arrival and were never validatable before, so stale values are a
// pre-existing condition. The plan adds real FKs only once this report is
// clean.
func (p *backfillGroupsCmd) reportDanglingRefs(ctx context.Context, target *sql.DB, merged map[string]*mergedGroup) {
	report := func(query, kind string) {
		rows, err := target.QueryContext(ctx, query)
		if err != nil {
			p.logger.Warn().Err(err).Str("kind", kind).Msg("dangling-reference check failed")
			return
		}
		defer func() { _ = rows.Close() }()
		n := 0
		for rows.Next() {
			var ref, where string
			if err := rows.Scan(&ref, &where); err != nil {
				p.logger.Warn().Err(err).Str("kind", kind).Msg("dangling-reference scan failed")
				return
			}
			if _, ok := merged[ref]; ok {
				continue
			}
			n++
			p.logger.Warn().Str("group_id", ref).Str("held_by", where).Str("kind", kind).
				Msg("references a group id that exists in neither source — stale, predates this backfill")
		}
		if err := rows.Err(); err != nil {
			p.logger.Warn().Err(err).Str("kind", kind).Msg("dangling-reference check failed")
			return
		}
		if n == 0 {
			p.logger.Info().Str("kind", kind).Msg("no dangling group references")
		}
	}
	report(`SELECT DISTINCT unnest(scope_group_ids), 'membership tenant=' || tenant_id || ' wallet=' || wallet
	          FROM memberships WHERE scope_group_ids IS NOT NULL`, "memberships.scope_group_ids")
	report(`SELECT DISTINCT unnest(scope_group_ids), 'invitation tenant=' || tenant_id || ' email=' || email
	          FROM invitations WHERE scope_group_ids IS NOT NULL`, "invitations.scope_group_ids")
	report(`SELECT DISTINCT source_group_id, 'entitlement tenant=' || tenant_id || ' vehicle=' || vehicle_token_id
	          FROM vehicle_entitlements WHERE source_group_id IS NOT NULL`, "vehicle_entitlements.source_group_id")
}

func sortedGroupIDs(merged map[string]*mergedGroup) []string {
	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func readGroups(ctx context.Context, src *sql.DB, query, source string) ([]srcGroup, error) {
	rows, err := src.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []srcGroup
	for rows.Next() {
		g := srcGroup{source: source}
		if err := rows.Scan(&g.id, &g.tenantID, &g.name, &g.color, &g.createdAt, &g.updatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func readMemberships(ctx context.Context, src *sql.DB, query string) ([]groupMembership, error) {
	rows, err := src.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []groupMembership
	for rows.Next() {
		var m groupMembership
		if err := rows.Scan(&m.tenantID, &m.tokenID, &m.groupID, &m.createdAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
