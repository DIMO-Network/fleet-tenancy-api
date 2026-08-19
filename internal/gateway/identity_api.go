// Package gateway holds the clients for the DIMO platform services this
// service consumes. It is a consumer of those services, never a mirror: nothing
// fetched here is persisted beyond a cache.
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
