package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	shttp "github.com/DIMO-Network/shared/pkg/http"
	"github.com/rs/zerolog"
)

const accountsAPITimeout = 60 * time.Second

// ErrAccountNotFound is the Accounts service's 404: no account exists for the
// email. Provisioning distinguishes it from every other failure — this one
// means "create it", the rest mean "stop".
var ErrAccountNotFound = errors.New("account not found")

// AccountsAPI is the slice of the DIMO Accounts service that provisioning
// needs: resolve an email to a wallet, creating the account when it does not
// exist. Ported from kaufmann-oracle's client, trimmed to lookups by email —
// this service identifies people by wallet everywhere else and has no reason to
// look one up.
type AccountsAPI interface {
	// GetAccountByEmail resolves an email to its account. developerJWT is
	// required: the Accounts service only echoes walletAddress (the Extended
	// response) to allowlisted developer-license callers, and an account
	// without a wallet is useless to a membership write.
	GetAccountByEmail(email, developerJWT string) (*Account, error)
	// CreateAccount registers a new account for the email, associating
	// providedSignerAddress — the acting tenant's signer — so the tenant can
	// operate on the account's kernel. The Accounts service requires the
	// developer JWT on this endpoint.
	CreateAccount(email, providedSignerAddress, developerJWT string) (*Account, error)
}

// Account is the subset of both Accounts-service response shapes that
// provisioning consumes. GET and POST return different payloads upstream; both
// carry the wallet, which is the only thing a membership needs.
type Account struct {
	WalletAddress string `json:"walletAddress"`
}

type accountsAPIService struct {
	baseURL url.URL
	logger  zerolog.Logger
}

func NewAccountsAPIService(logger *zerolog.Logger, baseURL url.URL) AccountsAPI {
	return &accountsAPIService{
		baseURL: baseURL,
		logger:  logger.With().Str("component", "accounts-api").Logger(),
	}
}

func (a *accountsAPIService) GetAccountByEmail(email, developerJWT string) (*Account, error) {
	if email == "" {
		return nil, errors.New("email is required")
	}
	if a.baseURL.String() == "" {
		return nil, fmt.Errorf("ACCOUNTS_API_ENDPOINT is not configured")
	}

	headers := map[string]string{"Authorization": "Bearer " + developerJWT}
	hcw, _ := shttp.NewClientWrapper(a.baseURL.String(), "", accountsAPITimeout, headers, true, shttp.WithRetry(3))

	q := url.Values{}
	q.Add("email", email)
	resp, err := hcw.ExecuteRequest("/api/account?"+q.Encode(), "GET", nil)
	if err != nil {
		var respErr shttp.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("accounts-api GET /api/account: %w", err)
	}
	return decodeAccount(resp.Body)
}

func (a *accountsAPIService) CreateAccount(email, providedSignerAddress, developerJWT string) (*Account, error) {
	if email == "" || providedSignerAddress == "" || developerJWT == "" {
		return nil, errors.New("email, providedSignerAddress and developerJWT are required")
	}
	if a.baseURL.String() == "" {
		return nil, fmt.Errorf("ACCOUNTS_API_ENDPOINT is not configured")
	}

	headers := map[string]string{"Authorization": "Bearer " + developerJWT}
	hcw, _ := shttp.NewClientWrapper(a.baseURL.String(), "", accountsAPITimeout, headers, true, shttp.WithRetry(3))

	payload, err := json.Marshal(map[string]string{
		"email":                 email,
		"providedSignerAddress": providedSignerAddress,
	})
	if err != nil {
		return nil, err
	}
	resp, err := hcw.ExecuteRequest("/api/shared/account/email", "POST", payload)
	if err != nil {
		return nil, fmt.Errorf("accounts-api POST /api/shared/account/email: %w", err)
	}
	return decodeAccount(resp.Body)
}

func decodeAccount(body io.ReadCloser) (*Account, error) {
	defer body.Close() //nolint:errcheck
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("accounts-api response: %w", err)
	}
	var acct Account
	if err := json.Unmarshal(raw, &acct); err != nil {
		return nil, fmt.Errorf("accounts-api response: %w", err)
	}
	return &acct, nil
}
