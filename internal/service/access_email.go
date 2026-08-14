package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/DIMO-Network/fleet-tenancy-api/internal/config"
	"github.com/DIMO-Network/fleet-tenancy-api/internal/gateway"
	"github.com/rs/zerolog"
)

// templatedSender is the slice of gateway.PostmarkAPI this service needs — an
// interface so the provision flow is testable without Postmark.
type templatedSender interface {
	Enabled() bool
	SendTemplated(from, to, templateAlias string, model any) error
}

// AccessEmailService sends the one email this service sends: "you've been
// given access", when a member is provisioned into a tenant. It lives here —
// in the service that owns provisioning — so every provisioning surface (the
// operator console today, fleet-lite's member flows when they converge on
// /user/v1) shares one notification rather than each caller growing its own.
// Before this, only kaufmann's legacy customer-account path emailed anyone;
// the console and admin-grant paths provisioned people silently.
type AccessEmailService struct {
	logger   *zerolog.Logger
	sender   templatedSender
	from     string
	alias    string
	loginURL string
}

// accessGrantedModel is substituted into the Postmark template. Field names
// must match the {{mustache}} variables in templates/postmark/access-granted.*.
type accessGrantedModel struct {
	TenantName string `json:"tenant_name"`
	LoginURL   string `json:"login_url"`
}

const defaultProvisionTemplateAlias = "fleet-access-granted"

func NewAccessEmailService(logger *zerolog.Logger, settings *config.Settings) *AccessEmailService {
	alias := settings.ProvisionTemplateAlias
	if alias == "" {
		alias = defaultProvisionTemplateAlias
	}
	return &AccessEmailService{
		logger:   logger,
		sender:   gateway.NewPostmarkAPI(*logger, settings.PostmarkServerToken),
		from:     settings.ProvisionEmailFrom,
		alias:    alias,
		loginURL: strings.TrimSuffix(settings.FleetAppBaseURL.String(), "/"),
	}
}

// Enabled reports whether sending can happen at all: a token and a from
// address. Callers use it to report emailSent honestly rather than counting a
// skipped send as one that happened.
func (s *AccessEmailService) Enabled() bool {
	return s.sender.Enabled() && s.from != ""
}

// SendAccessGranted emails the person that tenantName's fleet is now open to
// them and where to sign in. The caller decides what a failure means — for
// provisioning that is "the grant stands, the response says the email did
// not go out".
func (s *AccessEmailService) SendAccessGranted(_ context.Context, toEmail, tenantName string) error {
	if !s.Enabled() {
		return fmt.Errorf("access email is not configured")
	}
	return s.sender.SendTemplated(s.from, toEmail, s.alias, accessGrantedModel{
		TenantName: tenantName,
		LoginURL:   s.loginURL,
	})
}
