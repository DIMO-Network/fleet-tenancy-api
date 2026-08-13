package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
)

// AttestAPI delivers signed CloudEvents to DIMO's attest ingest — the outward
// half of the group-attestation publisher. One event per request, posted to
// the bare base URL, exactly as both source apps did.
type AttestAPI interface {
	SubmitCloudEvent(ctx context.Context, developerJWT string, event []byte) error
}

type attestAPIService struct {
	logger  *zerolog.Logger
	baseURL string
	client  *http.Client
}

// attestTimeout matches kaufmann's publisher (15s with retries handled by the
// caller's scan cadence rather than in-line).
const attestTimeout = 15 * time.Second

func NewAttestAPIService(logger *zerolog.Logger, baseURL string) AttestAPI {
	return &attestAPIService{
		logger:  logger,
		baseURL: baseURL,
		client:  &http.Client{Timeout: attestTimeout},
	}
}

func (s *attestAPIService) SubmitCloudEvent(ctx context.Context, developerJWT string, event []byte) error {
	if s.baseURL == "" {
		return fmt.Errorf("ATTEST_API_URL is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL, bytes.NewReader(event))
	if err != nil {
		return fmt.Errorf("build attest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/cloudevents+json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+developerJWT)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("attest request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("attest API returned %d: %s", resp.StatusCode, body)
	}
	// Drain for connection reuse.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}
