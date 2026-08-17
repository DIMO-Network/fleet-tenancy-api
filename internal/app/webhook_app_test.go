package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The webhook listener exists to be publicly reachable, so what it does NOT
// serve is the security property — and a property that only holds because the
// ingress is written correctly is not one this repo should rely on. These
// assert it structurally: the routes simply are not registered here, so a
// widened ingress rule reaches nothing.
//
// If someone ever registers a /v1 route on this app, this test fails and the
// comment in WebhookApp explains why that is not a test to update.
func TestWebhookAppServesNothingButTheWebhook(t *testing.T) {
	logger := zerolog.Nop()
	settings := &config.Settings{PostmarkWebhookSecret: "a-secret-long-enough-to-be-real"}
	// pdb is nil: none of the paths asserted below reach the database — an
	// unauthenticated request is refused before the handler does any work.
	app := WebhookApp(settings, &logger, nil)

	// Every internal surface, as the callers actually build the paths.
	internal := []struct{ method, path string }{
		{http.MethodGet, "/v1/authz?wallet=0x1&tenant_id=t"},
		{http.MethodGet, "/v1/tenants"},
		{http.MethodGet, "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/members"},
		{http.MethodPut, "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/members/0x1"},
		{http.MethodPost, "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/members/provision"},
		{http.MethodGet, "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/dimo-token"},
		{http.MethodGet, "/v1/tenants/aaaaaaaa-0000-0000-0000-000000000001/invitations"},
		{http.MethodPost, "/v1/invitations/accept"},
		{http.MethodGet, "/v1/resolve/client-id/0x1"},
		{http.MethodPost, "/v1/tenants"},
		{http.MethodGet, "/version"},
	}
	for _, tc := range internal {
		t.Run("404 for "+tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req, -1)
			require.NoError(t, err)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode,
				"the public listener must not serve any internal surface — "+
					"this is the reason it is a separate listener at all")
		})
	}
}

func TestWebhookAppServesTheWebhookAndAHealthProbe(t *testing.T) {
	logger := zerolog.Nop()
	settings := &config.Settings{PostmarkWebhookSecret: "a-secret-long-enough-to-be-real"}
	app := WebhookApp(settings, &logger, nil)

	t.Run("health answers without credentials", func(t *testing.T) {
		// The ingress health-checks this: a probe must not have to present the
		// webhook credential, nor be counted as a malformed event.
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/health", nil), -1)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("the webhook is registered and still authenticates", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, -1)
		require.NoError(t, err)
		// 403, not 404: the route exists, and the credential is what refused it.
		// The listener split is defence in depth, never the defence.
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}
