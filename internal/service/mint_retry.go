package service

import (
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenMinter is the one method this service needs from dimoauth.AuthService,
// narrowed so the retry below is testable without an auth server.
type tokenMinter interface {
	GetToken() *jwt.Token
}

// mintAttempts is how many times a mint is tried before it is called a
// failure, and mintBackoff is the pause between attempts.
//
// Three, not more: this runs inside an HTTP handler on the operator console's
// path, so the worst case must stay inside a human's patience. Two attempts
// would already remove most of the flake; the third is for the case where the
// first retry lands on the same unlucky replica.
const (
	mintAttempts = 3
	mintBackoff  = 250 * time.Millisecond
)

// mintWithRetry gets a developer JWT, retrying a nil result.
//
// WHY THIS EXISTS, because it looks like the kind of retry that papers over a
// real error. On 2026-08-20 fleet-lite's nightly groups-diff failed on
// `submit_challenge` with 400 "Could not verify signature" for one tenant; the
// next run succeeded for that tenant and failed for a DIFFERENT one, and the
// run after that was clean. The key is right — the same key mints successfully
// seconds later, and identity-api confirms the licence's signer has not
// changed since the tenant was created. It is the login challenge that is
// unreliable, roughly one attempt in six across six tenants.
//
// A retry is the correct response specifically because the challenge is
// SINGLE-USE. `dimoauth` already retries the two HTTP calls individually
// (shttp.WithRetry(3)), which cannot help and may be the cause: re-submitting a
// consumed or unknown `state` is exactly what "could not verify signature"
// looks like from outside. Only a fresh challenge can succeed, and `GetToken`
// starts one on every call — so calling it again is a new attempt, not the same
// request repeated.
//
// It deliberately does NOT retry forever, and it does not swallow the failure:
// nil still comes back after the last attempt, and the caller still errors. A
// credential that is genuinely wrong fails in about half a second rather than
// hanging.
func mintWithRetry(minter tokenMinter, onRetry func(attempt int)) *jwt.Token {
	for attempt := 1; ; attempt++ {
		if token := minter.GetToken(); token != nil {
			return token
		}
		if attempt >= mintAttempts {
			return nil
		}
		if onRetry != nil {
			onRetry(attempt)
		}
		time.Sleep(mintBackoff)
	}
}
