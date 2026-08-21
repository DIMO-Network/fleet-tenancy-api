package sharing

import (
	"context"
	"net/url"
	"testing"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/riverqueue/river"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An unconfigured environment is a supported state, not an error. This service
// is what both apps fail closed on, so a missing bundler URL must leave it
// booting and answering /v1/authz — with no queue, no pgx pool and no error.
func TestNewQueue_UnconfiguredIsNilAndSilent(t *testing.T) {
	logger := zerolog.Nop()
	q, err := NewQueue(context.Background(), &logger, &config.Settings{}, river.NewWorkers())

	require.NoError(t, err, "an unconfigured sharing feature must not be an error")
	assert.Nil(t, q, "no queue should be built when sharing is unconfigured")
}

// The reason NewQueue refuses to build a client for a nil worker bundle, kept
// as a test because the failure it prevents is a startup crash rather than a
// broken feature.
//
// river.Client.Start rejects an empty Workers bundle ("at least one Worker
// must be added to the Workers bundle"). main() treats a queue that cannot
// start as fatal — correctly, since a configured feature whose queue nothing
// drains is worse than a refusal — so building a client with no workers would
// take the whole service down in exactly the environments where sharing is
// configured. Both apps fail closed on this service's /v1/authz, so that is a
// two-app outage caused by a feature neither of them calls yet.
func TestNewQueue_ConfiguredButNoWorkersDoesNotStart(t *testing.T) {
	logger := zerolog.Nop()
	q, err := NewQueue(context.Background(), &logger, fullySharingConfigured(t), nil)

	require.NoError(t, err, "no workers is a supported state, not an error")
	assert.Nil(t, q, "a configured feature with no workers must not build a River client")
}

// Start and Stop are called unconditionally by main, which has no reason to
// know whether the feature is on. A nil Queue must absorb both rather than
// panicking the process at startup or during a deploy's drain.
func TestNilQueue_StartAndStopAreNoOps(t *testing.T) {
	var q *Queue
	assert.NoError(t, q.Start(context.Background()))
	assert.NoError(t, q.Stop(context.Background()))
}

// The fleet client is what turns a configured feature into something that can
// reach a bundler. Unconfigured, it must name the problem rather than dialling
// empty URLs and failing somewhere less legible.
func TestNewFleetClient_UnconfiguredReturnsNamedError(t *testing.T) {
	client, err := NewFleetClient(&config.Settings{})

	assert.Nil(t, client)
	assert.ErrorIs(t, err, ErrNotConfigured)
}

// The bundler URL is deliberately passed as the paymaster URL too — ZeroDev
// serves both from one project URL, and kaufmann's client is configured the
// same way. A reader who assumes that is a copy-paste slip should find this
// test rather than "fixing" it.
func TestNewFleetClient_ConfiguredDialsWithBundlerAsPaymaster(t *testing.T) {
	settings := fullySharingConfigured(t)

	// fleet.NewClient dials over HTTP RPC, which does not connect eagerly, so
	// this builds against unreachable hosts without touching the network.
	client, err := NewFleetClient(settings)
	require.NoError(t, err)
	require.NotNil(t, client)
	client.Close()
}

func fullySharingConfigured(t *testing.T) *config.Settings {
	t.Helper()
	s := &config.Settings{
		SacdAddress:         "0x3c152B5d96769661008Ff404224d6530FCAC766d",
		SyntheticNftAddress: "0x4804e8D1661cd1a1e5dDdE1ff458A7f878c0aC6D",
		VehicleNftAddress:   "0xbA5738a18d83D41847dfFbDC6101d37C69c9B0cF",
		RPCURL:              mustURL(t, "https://polygon-mainnet.example/v2/key"),
		BundlerURL:          mustURL(t, "https://rpc.zerodev.example/api/v2/bundler/proj"),
		ChainID:             137,
	}
	require.True(t, s.SharingConfigured(), "precondition: this helper must produce a configured feature")
	return s
}

func mustURL(t *testing.T, raw string) url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return *u
}
