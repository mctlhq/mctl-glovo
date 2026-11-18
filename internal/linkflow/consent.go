package linkflow

import (
	"context"
	"errors"

	"github.com/mctlhq/mctl-glovo/internal/consentgate"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// ConsentVersion is the version of the consent text this service shows on
// /link. A code is issued only when mctl-api's consent record for the
// caller names exactly this version: a consent to an older text is not a
// consent to this one.
const ConsentVersion = "c2-link-v2"

// ConsentText is what linking allows, shown on /link with ConsentVersion.
// Changing a word here means a new ConsentVersion, and the same version in
// mctl-api's internal/glovoconsent (until both match no link code is issued).
// Every capability it names must be a tool that tools/list serves
// (cmd/mctl-glovo TestConsentTextMatchesTheServedTools).
var ConsentText = []string{
	"Linking stores your Glovo session (access and refresh tokens and the device id) encrypted with a key held in Vault. Only the mctl-glovo service can decrypt it, and only for requests made with your MCTL account.",
	"The session is used only to read your Glovo account: your order history, your basket, your saved locations and stores, so that your assistant can analyse your orders and plan a reorder. It is never used to change your basket, to place an order or to pay.",
	"You can unlink at any time. Unlinking deletes the stored session at once; encrypted database backups age out within 14 days.",
	"MCTL cannot revoke the session at Glovo. After unlinking, sign out of other sessions in Glovo; after a suspected breach, also change your Glovo password.",
}

// ConsentSource reads the consent mctl-api recorded for a principal. It is
// server-side: the consent version never comes from the request.
type ConsentSource interface {
	// RecordedConsent returns the consent version recorded for pid.
	// ErrNoConsent is the documented absence (no record); any other error
	// means the record could not be read. Both refuse the code.
	RecordedConsent(ctx context.Context, pid tokenstore.PID) (string, error)
}

var (
	// ErrNoConsent means mctl-api has no consent record for the principal.
	// It is consentgate.ErrNoConsent, so the gate reads it as a definite no.
	ErrNoConsent = consentgate.ErrNoConsent
	// ErrConsentUnavailable means the consent record could not be read.
	ErrConsentUnavailable = errors.New("linkflow: consent record unavailable")
)

// UnavailableConsent is the consent source until mctl-api serves consent
// records (slice C2-1c). It never reports a consent, so no code is issued.
type UnavailableConsent struct{}

// RecordedConsent implements ConsentSource.
func (UnavailableConsent) RecordedConsent(context.Context, tokenstore.PID) (string, error) {
	return "", ErrConsentUnavailable
}
