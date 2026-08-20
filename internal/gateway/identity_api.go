// Package gateway holds the clients for the DIMO platform services this
// service consumes. It is a consumer of those services, never an authority:
// nothing fetched here is authored locally. Since plan 07 step 3 one read IS
// persisted — the vehicle roster — but as a reconciled cache of the chain's
// answer, re-read on a schedule and never written by a local action, which is
// the distinction that matters. See internal/db/migrations, table `vehicles`.
package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	shttp "github.com/DIMO-Network/shared/pkg/http"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog"
)

const identityAPITimeout = 30 * time.Second

// ErrVehicleNotFound is identity-api answering that no vehicle exists for the
// token id. Distinct from a transport failure: this one means the caller asked
// about something that is not there, which is a 404, not a retry.
var ErrVehicleNotFound = errors.New("vehicle not found")

// IdentityAPI answers the questions this service has for identity-api.
//
// RedirectURIForClientID: minting a developer JWT signs a challenge whose
// domain must be a registered redirect URI, and the URI is platform state — it
// lives on the license, not in tenant_credentials, so it is looked up rather
// than stored and re-synchronised.
//
// VehicleOwner: who currently owns a vehicle NFT, for vehicle sharing. Read
// live and never cached, because it decides whose kernel account a UserOp is
// sent from — a stale owner means signing against an account that no longer
// holds the vehicle.
type IdentityAPI interface {
	RedirectURIForClientID(clientID string) (string, error)
	VehicleOwner(tokenID int64) (string, error)
	PrivilegedVehicles(clientID string) ([]RosterVehicle, error)
	VehicleDetail(tokenID int64) (*RosterVehicle, error)
}

// RosterVehicle is one identity-api vehicle node, reduced to the fields the
// roster holds. Deliberately not the full node: a field this service does not
// store is a field it cannot serve stale.
type RosterVehicle struct {
	TokenID      int64
	Owner        string
	DefinitionID string
	Make         string
	Model        string
	Year         int
	MintedAt     *time.Time
}

type identityAPIService struct {
	endpoint url.URL
	logger   zerolog.Logger
}

func NewIdentityAPIService(logger *zerolog.Logger, endpoint url.URL) IdentityAPI {
	return &identityAPIService{
		endpoint: endpoint,
		logger:   logger.With().Str("component", "identity-api").Logger(),
	}
}

// developerLicenseQuery matches kaufmann-oracle's, minus the fields nothing
// here reads. first: 1 because only the first URI is used — the same choice
// kaufmann makes when it mints.
const developerLicenseQuery = `{
	developerLicense(by: {clientId: %q}) {
		redirectURIs(first: 1) {
			edges { node { uri } }
		}
	}
}`

func (i *identityAPIService) RedirectURIForClientID(clientID string) (string, error) {
	if i.endpoint.String() == "" {
		return "", fmt.Errorf("IDENTITY_API_ENDPOINT is not configured")
	}
	payload, err := json.Marshal(map[string]string{
		"query": fmt.Sprintf(developerLicenseQuery, clientID),
	})
	if err != nil {
		return "", err
	}

	hcw, _ := shttp.NewClientWrapper(i.endpoint.String(), "", identityAPITimeout, nil, true, shttp.WithRetry(2))
	resp, err := hcw.ExecuteRequest("", "POST", payload)
	if err != nil {
		return "", fmt.Errorf("identity-api query: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("identity-api response: %w", err)
	}

	var out struct {
		Data struct {
			DeveloperLicense struct {
				RedirectURIs struct {
					Edges []struct {
						Node struct {
							URI string `json:"uri"`
						} `json:"node"`
					} `json:"edges"`
				} `json:"redirectURIs"`
			} `json:"developerLicense"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("identity-api response: %w", err)
	}
	edges := out.Data.DeveloperLicense.RedirectURIs.Edges
	if len(edges) == 0 || edges[0].Node.URI == "" {
		return "", fmt.Errorf("developer license %s has no registered redirect URI", clientID)
	}
	return edges[0].Node.URI, nil
}

// vehicleOwnerQuery reads the current on-chain owner of a vehicle NFT.
const vehicleOwnerQuery = `{
	vehicle(tokenId: %d) {
		owner
	}
}`

// VehicleOwner returns the vehicle's current owner, EIP-55 checksummed.
//
// The owner is the account a share's UserOperation is sent FROM, so this is
// asked at the moment of use and never cached or taken from the caller.
// Ownership can change between a page render and a click, and acting on a
// stale answer would mean signing against an account that no longer holds the
// vehicle — or, worse, still holds our signer while no longer being the party
// the customer meant to share.
func (i *identityAPIService) VehicleOwner(tokenID int64) (string, error) {
	if i.endpoint.String() == "" {
		return "", fmt.Errorf("IDENTITY_API_ENDPOINT is not configured")
	}
	payload, err := json.Marshal(map[string]string{
		"query": fmt.Sprintf(vehicleOwnerQuery, tokenID),
	})
	if err != nil {
		return "", err
	}

	hcw, _ := shttp.NewClientWrapper(i.endpoint.String(), "", identityAPITimeout, nil, true, shttp.WithRetry(2))
	resp, err := hcw.ExecuteRequest("", "POST", payload)
	if err != nil {
		return "", fmt.Errorf("identity-api vehicle query: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("identity-api response: %w", err)
	}

	var out struct {
		Data struct {
			Vehicle *struct {
				Owner string `json:"owner"`
			} `json:"vehicle"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("identity-api response: %w", err)
	}
	if out.Data.Vehicle == nil || out.Data.Vehicle.Owner == "" {
		return "", ErrVehicleNotFound
	}
	if !common.IsHexAddress(out.Data.Vehicle.Owner) {
		return "", fmt.Errorf("identity-api returned a malformed owner %q for vehicle %d",
			out.Data.Vehicle.Owner, tokenID)
	}
	// Checksummed here so every comparison downstream is against one form.
	return common.HexToAddress(out.Data.Vehicle.Owner).Hex(), nil
}

// privilegedVehiclesQuery lists the vehicles a developer-license client id is
// privileged on, one page at a time.
//
// Format args: 1=clientID, 2=page size, 3=after-cursor (a quoted string, or the
// bare word null for the first page).
//
// Shaped after fleet-lite's VehiclesByPrivilegeAndCursorQuery, minus the device
// nodes: the roster records what a vehicle is and who owns it, and a paired
// device is neither. Asking for less also keeps the sweep's response size down,
// and it is swept over every tenant's licence.
const privilegedVehiclesQuery = `{
	vehicles(filterBy: {privileged: %q}, first: %d, after: %s) {
		nodes {
			tokenId
			owner
			mintedAt
			definition { id make model year }
		}
		pageInfo { hasNextPage endCursor }
	}
}`

// privilegedVehiclesPageSize is identity-api's maximum. The sweep is over every
// tenant licence, so halving it doubles the round trips for the whole platform.
const privilegedVehiclesPageSize = 100

// privilegedVehiclesMaxPages bounds the cursor loop. A server that returned
// hasNextPage forever — or a cursor that failed to advance — would otherwise
// spin until the job's activeDeadlineSeconds killed it, with no clue why. At
// 100 per page this ceiling is far above any licence we hold, so hitting it is
// a bug rather than a big customer, and it is reported as one.
const privilegedVehiclesMaxPages = 200

// PrivilegedVehicles returns every vehicle a client id is privileged on
// (SACD-shared), following the cursor to exhaustion.
//
// A vehicle appearing under two licences is returned by both calls; the roster
// is keyed by token id and upserts, so the duplicate collapses there rather
// than needing a platform-wide set here.
func (i *identityAPIService) PrivilegedVehicles(clientID string) ([]RosterVehicle, error) {
	if i.endpoint.String() == "" {
		return nil, fmt.Errorf("IDENTITY_API_ENDPOINT is not configured")
	}
	if clientID == "" {
		return nil, fmt.Errorf("client id is required")
	}

	var out []RosterVehicle
	after := "null"
	for page := 0; ; page++ {
		if page >= privilegedVehiclesMaxPages {
			return nil, fmt.Errorf("privileged vehicles for %s: exceeded %d pages, refusing to loop",
				clientID, privilegedVehiclesMaxPages)
		}

		payload, err := json.Marshal(map[string]string{
			"query": fmt.Sprintf(privilegedVehiclesQuery, clientID, privilegedVehiclesPageSize, after),
		})
		if err != nil {
			return nil, err
		}

		hcw, _ := shttp.NewClientWrapper(i.endpoint.String(), "", identityAPITimeout, nil, true, shttp.WithRetry(2))
		resp, err := hcw.ExecuteRequest("", "POST", payload)
		if err != nil {
			return nil, fmt.Errorf("identity-api privileged vehicles: %w", err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close() //nolint:errcheck
		if err != nil {
			return nil, fmt.Errorf("identity-api response: %w", err)
		}

		var res struct {
			Data struct {
				Vehicles struct {
					Nodes []struct {
						TokenID    int64   `json:"tokenId"`
						Owner      string  `json:"owner"`
						MintedAt   *string `json:"mintedAt"`
						Definition struct {
							ID    string `json:"id"`
							Make  string `json:"make"`
							Model string `json:"model"`
							Year  int    `json:"year"`
						} `json:"definition"`
					} `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"vehicles"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("identity-api response: %w", err)
		}
		// A GraphQL 200 carrying errors is a failure, and a silent one if the
		// nodes list is simply read as empty. For a sweep that decides which
		// vehicles we can still see, "no results" and "the query broke" must
		// never be the same outcome.
		if len(res.Errors) > 0 {
			return nil, fmt.Errorf("identity-api privileged vehicles for %s: %s",
				clientID, res.Errors[0].Message)
		}

		for _, n := range res.Data.Vehicles.Nodes {
			v := RosterVehicle{
				TokenID:      n.TokenID,
				DefinitionID: n.Definition.ID,
				Make:         n.Definition.Make,
				Model:        n.Definition.Model,
				Year:         n.Definition.Year,
			}
			// A malformed owner is dropped rather than stored. The roster's
			// whole purpose is that this column is trustworthy; writing
			// something that is not an address would make it exactly as
			// reliable as the copy it replaces.
			if common.IsHexAddress(n.Owner) {
				v.Owner = common.HexToAddress(n.Owner).Hex()
			} else if n.Owner != "" {
				i.logger.Warn().Int64("token_id", n.TokenID).Str("owner", n.Owner).
					Msg("identity-api returned a malformed owner; leaving it unset")
			}
			if n.MintedAt != nil && *n.MintedAt != "" {
				if ts, terr := time.Parse(time.RFC3339, *n.MintedAt); terr == nil {
					v.MintedAt = &ts
				}
			}
			out = append(out, v)
		}

		pi := res.Data.Vehicles.PageInfo
		// An empty end cursor with hasNextPage set would re-request page one
		// forever. Stop and say so rather than sweep the same page until the
		// deadline.
		if !pi.HasNextPage {
			return out, nil
		}
		if pi.EndCursor == "" {
			return nil, fmt.Errorf("privileged vehicles for %s: hasNextPage with no cursor", clientID)
		}
		after = fmt.Sprintf("%q", pi.EndCursor)
	}
}

// vehicleDetailQuery reads one vehicle's roster fields by token id.
//
// Deliberately NOT privilege-filtered: what a vehicle is and who owns it is
// public chain data, and identity-api answers for a token no licence of ours is
// privileged on. That is what lets the roster hold a vehicle the privileged
// sweep cannot enumerate.
const vehicleDetailQuery = `{
	vehicle(tokenId: %d) {
		tokenId
		owner
		mintedAt
		definition { id make model year }
	}
}`

// VehicleDetail returns one vehicle's roster fields, or ErrVehicleNotFound.
//
// The sweep enumerates; this fills. A vehicle can be entitled to a customer
// while its SACD is not shared with any licence we hold — the entitlement is
// this service's own record, so we know the token id without being able to list
// it — and such a vehicle must still be in the roster. An entitled vehicle
// missing from the roster would be the empty-fleet incident again, one layer
// down, once readers cut over to it in step 4.
func (i *identityAPIService) VehicleDetail(tokenID int64) (*RosterVehicle, error) {
	if i.endpoint.String() == "" {
		return nil, fmt.Errorf("IDENTITY_API_ENDPOINT is not configured")
	}
	payload, err := json.Marshal(map[string]string{
		"query": fmt.Sprintf(vehicleDetailQuery, tokenID),
	})
	if err != nil {
		return nil, err
	}

	hcw, _ := shttp.NewClientWrapper(i.endpoint.String(), "", identityAPITimeout, nil, true, shttp.WithRetry(2))
	resp, err := hcw.ExecuteRequest("", "POST", payload)
	if err != nil {
		return nil, fmt.Errorf("identity-api vehicle detail: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("identity-api response: %w", err)
	}

	var res struct {
		Data struct {
			Vehicle *struct {
				TokenID    int64   `json:"tokenId"`
				Owner      string  `json:"owner"`
				MintedAt   *string `json:"mintedAt"`
				Definition struct {
					ID    string `json:"id"`
					Make  string `json:"make"`
					Model string `json:"model"`
					Year  int    `json:"year"`
				} `json:"definition"`
			} `json:"vehicle"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("identity-api response: %w", err)
	}
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("identity-api vehicle %d: %s", tokenID, res.Errors[0].Message)
	}
	if res.Data.Vehicle == nil {
		return nil, ErrVehicleNotFound
	}

	n := res.Data.Vehicle
	out := &RosterVehicle{
		TokenID:      n.TokenID,
		DefinitionID: n.Definition.ID,
		Make:         n.Definition.Make,
		Model:        n.Definition.Model,
		Year:         n.Definition.Year,
	}
	if common.IsHexAddress(n.Owner) {
		out.Owner = common.HexToAddress(n.Owner).Hex()
	}
	if n.MintedAt != nil && *n.MintedAt != "" {
		if ts, terr := time.Parse(time.RFC3339, *n.MintedAt); terr == nil {
			out.MintedAt = &ts
		}
	}
	return out, nil
}
