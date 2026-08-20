package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/models"
	"github.com/ethereum/go-ethereum/common"
	"github.com/lib/pq"
)

// Metadata reads the roster for a named set of token ids.
//
// Plan 07 step 4 — the read side of the table step 3 stood up. Rows come back
// only for tokens the roster holds; asking for a token it has never seen is
// not an error and produces no row. The caller's list is the set, this is the
// join, and the two must not be confused: see models.VehicleMetadataResult for
// why a missing row must never remove a vehicle from a rendered fleet.
//
// Ordered by token id so a response is stable between calls — the caller sorts
// for display anyway, but a stable order makes a diff of two responses mean
// something.
func (s *RosterService) Metadata(ctx context.Context, tokenIDs []int64) ([]models.VehicleMetadata, error) {
	if len(tokenIDs) == 0 {
		return []models.VehicleMetadata{}, nil
	}

	rows, err := s.pdb.DBS().Reader.QueryContext(ctx,
		`SELECT vehicle_token_id, COALESCE(owner, ''), COALESCE(definition_id, ''),
		        COALESCE(make, ''), COALESCE(model, ''), year, minted_at,
		        COALESCE(vin, ''), COALESCE(license_plate, ''), reconciled_at, unseen_since,
		        synthetic_device_token_id, aftermarket_device_token_id
		   FROM vehicles
		  WHERE vehicle_token_id = ANY($1)
		  ORDER BY vehicle_token_id`,
		pq.Array(tokenIDs))
	if err != nil {
		return nil, fmt.Errorf("read roster metadata: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	out := make([]models.VehicleMetadata, 0, len(tokenIDs))
	for rows.Next() {
		var (
			v            models.VehicleMetadata
			year         sql.NullInt16
			mintedAt     sql.NullTime
			reconciledAt time.Time
			unseenSince  sql.NullTime
			owner        string
			synthetic    sql.NullInt64
			aftermarket  sql.NullInt64
		)
		if err := rows.Scan(&v.VehicleTokenID, &owner, &v.DefinitionID, &v.Make, &v.Model,
			&year, &mintedAt, &v.VIN, &v.LicensePlate, &reconciledAt, &unseenSince,
			&synthetic, &aftermarket); err != nil {
			return nil, fmt.Errorf("scan roster metadata: %w", err)
		}
		if synthetic.Valid {
			id := synthetic.Int64
			v.SyntheticDeviceTokenID = &id
		}
		if aftermarket.Valid {
			id := aftermarket.Int64
			v.AftermarketDeviceTokenID = &id
		}
		v.Owner = checksumOwner(owner)
		if year.Valid {
			v.Year = int(year.Int16)
		}
		if mintedAt.Valid {
			t := mintedAt.Time.UTC()
			v.MintedAt = &t
		}
		v.ReconciledAt = reconciledAt.UTC()
		if unseenSince.Valid {
			t := unseenSince.Time.UTC()
			v.UnseenSince = &t
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// checksumOwner normalises a stored owner to EIP-55.
//
// identity-api already returns checksummed addresses, so this changes nothing
// today — it is here so that a row written by some future path in another case
// cannot make a caller's string comparison against its own stored address fail
// silently. An address that does not parse is passed through untouched rather
// than blanked: showing something wrong beats showing nothing at all when the
// alternative hides a vehicle.
func checksumOwner(owner string) string {
	if owner == "" || !common.IsHexAddress(owner) {
		return owner
	}
	return common.HexToAddress(owner).Hex()
}
