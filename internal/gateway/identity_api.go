// Package gateway holds the clients for the DIMO platform services this
// service consumes. It is a consumer of those services, never a mirror: nothing
// fetched here is persisted beyond a cache.
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"time"

	shttp "github.com/DIMO-Network/shared/pkg/http"
	"github.com/rs/zerolog"
)

const identityAPITimeout = 30 * time.Second

// IdentityAPI answers the one question this service has for identity-api:
// which redirect URI is registered for a developer license. Minting a developer
// JWT signs a challenge whose domain must be a registered redirect URI, and the
// URI is platform state — it lives on the license, not in tenant_credentials,
// so it is looked up rather than stored and re-synchronised.
type IdentityAPI interface {
	RedirectURIForClientID(clientID string) (string, error)
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
