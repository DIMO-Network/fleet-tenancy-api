package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const postmarkBaseURL = "https://api.postmarkapp.com"

// PostmarkAPI is a minimal client for Postmark's templated-email and template
// APIs, ported from fleet-lite-app's proven client and trimmed to what this
// service sends. Auth is the server token in X-Postmark-Server-Token.
//
// This service historically had no email capability at all, on purpose. The
// one email it now sends — "you've been given access" on provisioning — lives
// here rather than in a caller because provisioning lives here: every surface
// that provisions (the operator console today, fleet-lite's member flows when
// they converge on /user/v1) inherits one notification implementation instead
// of each caller growing its own.
type PostmarkAPI struct {
	logger      zerolog.Logger
	serverToken string
	baseURL     string
}

func NewPostmarkAPI(logger zerolog.Logger, serverToken string) *PostmarkAPI {
	// Trimmed because AWS Secrets Manager refuses empty strings, so "feature
	// off" is provisioned as whitespace — which must not count as a token.
	return &PostmarkAPI{logger: logger, serverToken: strings.TrimSpace(serverToken), baseURL: postmarkBaseURL}
}

// Enabled reports whether a server token is configured. When false, callers
// treat sending as a no-op — local dev, and any environment where the token
// has not been provisioned yet.
func (p *PostmarkAPI) Enabled() bool { return p.serverToken != "" }

// SendTemplated sends one templated email (POST /email/withTemplate). The
// template alias must exist in the Postmark server the token belongs to — see
// templates/postmark/ and the push-postmark-templates command.
func (p *PostmarkAPI) SendTemplated(from, to, templateAlias string, model any) error {
	if !p.Enabled() {
		p.logger.Info().Str("to", to).Str("template", templateAlias).
			Msg("postmark not configured; email skipped")
		return nil
	}
	payload := map[string]any{
		"From":          from,
		"To":            to,
		"TemplateAlias": templateAlias,
		"TemplateModel": model,
		"MessageStream": "outbound",
	}
	var resp struct {
		ErrorCode int    `json:"ErrorCode"`
		Message   string `json:"Message"`
	}
	if err := p.do("POST", "/email/withTemplate", payload, &resp); err != nil {
		return err
	}
	if resp.ErrorCode != 0 {
		return fmt.Errorf("postmark send error %d: %s", resp.ErrorCode, resp.Message)
	}
	return nil
}

// InvitationModel is substituted into the Postmark invitation template. Field
// names must match the {{mustache}} variables in the template — the aliases
// are fleet-lite's, living server-side in Postmark, so this shape is theirs.
type InvitationModel struct {
	TenantName string `json:"tenant_name"`
	AcceptURL  string `json:"accept_url"`
	Inviter    string `json:"inviter"`
	ExpiresIn  string `json:"expires_in"`
}

// SendInvitation sends one invitation email (POST /email/withTemplate) and
// returns Postmark's MessageID ("" when sending is disabled).
//
// metadata is attached to the message and echoed back verbatim in every
// webhook event (delivery/open/bounce) — the invitation id rides here so those
// events correlate back to the row, with the MessageID as the fallback key.
//
// Differs from SendTemplated in exactly what invitation tracking needs: the
// MessageID comes back, metadata goes out, and opens are tracked (per-message
// opt-in; best-effort, needs the client to load images).
func (p *PostmarkAPI) SendInvitation(from, to, templateAlias string, model InvitationModel, metadata map[string]string) (string, error) {
	if !p.Enabled() {
		// Local-dev sink: nothing can send, so surface the accept link in the
		// logs — copy it from there to exercise the accept flow without any
		// email infrastructure. The plaintext token is IN this link; that is
		// acceptable only because an unconfigured Postmark means local dev.
		p.logger.Info().Str("to", to).Str("template", templateAlias).
			Str("accept_url", model.AcceptURL).
			Msg("postmark not configured; invitation email skipped — use this accept link locally")
		return "", nil
	}
	payload := map[string]any{
		"From":          from,
		"To":            to,
		"TemplateAlias": templateAlias,
		"TemplateModel": model,
		"MessageStream": "outbound",
		"TrackOpens":    true,
	}
	if len(metadata) > 0 {
		payload["Metadata"] = metadata
	}
	var resp struct {
		ErrorCode int    `json:"ErrorCode"`
		Message   string `json:"Message"`
		MessageID string `json:"MessageID"`
	}
	if err := p.do("POST", "/email/withTemplate", payload, &resp); err != nil {
		return "", err
	}
	if resp.ErrorCode != 0 {
		return "", fmt.Errorf("postmark send error %d: %s", resp.ErrorCode, resp.Message)
	}
	return resp.MessageID, nil
}

// UpsertTemplate creates or updates a template by alias, for the
// push-postmark-templates command. Postmark has no single upsert call, so PUT
// (update by alias) falls back to POST (create) on error 1101 (unknown alias).
func (p *PostmarkAPI) UpsertTemplate(alias, name, subject, htmlBody, textBody string) error {
	if !p.Enabled() {
		return fmt.Errorf("postmark server token not configured")
	}
	body := map[string]any{
		"Name":         name,
		"Alias":        alias,
		"Subject":      subject,
		"HtmlBody":     htmlBody,
		"TextBody":     textBody,
		"TemplateType": "Standard",
	}
	var resp struct {
		ErrorCode int    `json:"ErrorCode"`
		Message   string `json:"Message"`
	}
	err := p.do("PUT", "/templates/"+alias, body, &resp)
	if err == nil && resp.ErrorCode == 0 {
		return nil
	}
	if err == nil && resp.ErrorCode != 1101 {
		return fmt.Errorf("postmark update template error %d: %s", resp.ErrorCode, resp.Message)
	}
	resp.ErrorCode, resp.Message = 0, ""
	if cerr := p.do("POST", "/templates", body, &resp); cerr != nil {
		return cerr
	}
	if resp.ErrorCode != 0 {
		return fmt.Errorf("postmark create template error %d: %s", resp.ErrorCode, resp.Message)
	}
	return nil
}

// do performs a JSON request against the Postmark API. A 5xx is an error;
// Postmark-level errors (ErrorCode in the body, often under HTTP 422) are left
// for the caller to inspect.
func (p *PostmarkAPI) do(method, path string, payload, out any) error {
	var reqBody io.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal postmark request: %w", err)
		}
		reqBody = bytes.NewBuffer(body)
	}
	req, err := http.NewRequest(method, p.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build postmark request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Postmark-Server-Token", p.serverToken)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("postmark request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read postmark response: %w", err)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("postmark status %d: %s", resp.StatusCode, string(respBytes))
	}
	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("parse postmark response (status %d): %w", resp.StatusCode, err)
		}
	}
	return nil
}
