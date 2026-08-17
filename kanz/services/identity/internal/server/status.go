package server

// ACCOUNT STATUS — THE WRITE SIDE OF identity.Status (#525).
//
// Before this, StatusDisabled was a state the platform HONOURED AND COULD NOT
// REACH. Two independent enforcement points refuse a disabled account —
// identity.Signer.Mint (token.go) and /login (server.go) — the column carries a
// CHECK constraint admitting exactly 'active' and 'disabled', and the only value
// anything in the module ever wrote was StatusActive, from UserFromInvite on the
// redeem path. So the only way to lock out an offboarded trader or a compromised
// credential was a human running UPDATE against the production database: not
// attributable, not repeatable, and not a control anybody could test.
//
// # WHAT A DISABLE COVERS, AND WHAT IT DOES NOT
//
// It stops the NEXT login and every token minted after it. It does NOT stop the
// session already in flight: identity.DefaultTokenTTL is 8h and the api-gateway
// verifies signatures against JWKS without ever reading identity's database, so
// an outstanding token keeps working until it expires.
//
// THAT IS DEFENSIBLE FOR AN OFFBOARDING AND NOT FOR A COMPROMISED CREDENTIAL,
// which is the case people reach for this control in — so the response says so
// out loud rather than leaving an operator to assume otherwise. Closing it is
// #525 step 4 (a disabled_at column plus a gateway check that refuses a token
// minted before it), which costs the gateway a read it does not currently make
// and is therefore a real architectural change, argued separately. Nothing here
// forecloses it: SetStatus already owns the one statement that would stamp the
// column.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	observationpb "github.com/eighred/kanz/kanz-schemas-go/observation/v1"

	"github.com/eighred/kanz/internal/identity"
)

// disableUser locks an account out of every future login.
func (s *Server) disableUser(w http.ResponseWriter, r *http.Request) {
	s.setStatus(w, r, identity.StatusDisabled)
}

// enableUser is the inverse, and it is not a convenience.
//
// Without it an account disabled by mistake — a typo'd subject, an offboarding
// that was reversed — needs exactly the hand-run UPDATE this route exists to
// eliminate, and the mistake would be the thing that forces someone back to the
// production DSN.
func (s *Server) enableUser(w http.ResponseWriter, r *http.Request) {
	s.setStatus(w, r, identity.StatusActive)
}

// setStatus is the shared body of both routes. The status is a constant supplied
// by the handler and never taken from the request, so no caller can name a state
// the platform does not enforce.
func (s *Server) setStatus(w http.ResponseWriter, r *http.Request, status identity.Status) {
	claims, ok := s.operator(w, r)
	if !ok {
		return
	}
	subject := strings.TrimSpace(r.PathValue("subject"))
	if subject == "" {
		writeErr(w, http.StatusBadRequest, "a subject is required")
		return
	}

	// THE ACCOUNT IS LOADED FIRST, TO CHECK ITS TENANT. identity's pool is
	// UNSCOPED by necessity (identity.WhyNoTenantScope — login must find an
	// account before it knows its tenant), so there is no RLS policy standing
	// behind this route. Without this check an operator of one fund could disable
	// a trader at another, which on a multi-tenant platform is a denial-of-service
	// against a customer by a customer.
	u, err := s.provisioning.Store.UserBySubject(r.Context(), subject)
	switch {
	case errors.Is(err, identity.ErrUserNotFound):
		s.notFound(w, claims, subject, "no such account")
		return
	case err != nil:
		s.logger.Error("cannot load the account whose status was to change",
			"subject", subject, "err", err)
		writeErr(w, http.StatusInternalServerError, "the account status was not changed")
		return
	case u.Tenant != claims.Tenant:
		// THE SAME 404 AS "NO SUCH ACCOUNT", DELIBERATELY. A 403 here would
		// separate "exists, elsewhere" from "does not exist", handing any operator
		// on the platform a probe for other funds' staff lists — the same
		// enumeration ErrLoginFailed exists to prevent, one authority level up.
		s.notFound(w, claims, subject, "account belongs to tenant "+u.Tenant)
		return
	}

	if err := s.provisioning.Store.SetStatus(r.Context(), subject, status, s.now().UTC()); err != nil {
		if errors.Is(err, identity.ErrUserNotFound) {
			// The account was removed between the load and the write. Zero rows
			// affected is reported as an error by SetStatus rather than as success,
			// which is what makes this reachable at all.
			s.notFound(w, claims, subject, "the account disappeared between the load and the write")
			return
		}
		s.logger.Error("account status change failed", "subject", subject,
			"status", string(status), "operator", claims.Subject, "err", err)
		writeErr(w, http.StatusInternalServerError, "the account status was not changed")
		return
	}

	at := s.now().UTC()
	if err := s.provisioning.Audit.Record(r.Context(), statusDecisionLog(claims, u, status, at)); err != nil {
		// THE STATUS CHANGE IS ALREADY DURABLE, so this is not a failure the
		// caller can retry into consistency — answering 500 would tell an operator
		// the disable did not happen when it did, and send them to the hand-run
		// UPDATE this route exists to remove. The record is lost and the loss is
		// LOUD: an ERROR line naming the change that went unaudited is the one
		// thing a reviewer can reconcile against later.
		s.logger.Error("account status changed WITHOUT an audit record — the change is durable and "+
			"unattributed; reconcile from this line",
			"subject", u.Subject, "tenant", u.Tenant, "status", string(status),
			"operator", claims.Subject, "at", at, "err", err)
	}

	s.logger.Info("account status changed", "subject", u.Subject, "tenant", u.Tenant,
		"status", string(status), "operator", claims.Subject)

	writeJSON(w, http.StatusOK, map[string]any{
		"subject":    u.Subject,
		"tenant":     u.Tenant,
		"status":     string(status),
		"changed_by": claims.Subject,
		"changed_at": at,
		// SAID IN THE RESPONSE, not only in a comment nobody reading the API will
		// see. An operator disabling a compromised credential must not believe the
		// session in flight is over.
		"note": "a token already issued to this account stays valid until it expires; this stops " +
			"the next login, not the session in flight (#525 step 4)",
	})
}

// notFound answers both "no such account" and "not your tenant" identically, and
// logs which it actually was.
//
// The reason must reach an operator investigating and must not reach the caller,
// for the reason setStatus states at the call site.
func (s *Server) notFound(w http.ResponseWriter, claims *identity.Claims, subject, reason string) {
	s.logger.Warn("account status change refused", "subject", subject,
		"operator", claims.Subject, "operator_tenant", claims.Tenant, "reason", reason)
	writeErr(w, http.StatusNotFound, "no such account")
}

// statusDecisionLog is the audit record: WHO disabled WHOM, and WHEN.
//
// observation.v1.DecisionLog is reused rather than a bespoke shape — it is what
// pkg/auth already emits for every authorization decision and what AUDIT-01's
// projection already materializes into the queryable store (services/audit/
// internal/audit/classify.go), so this record lands in the same place an auditor
// already looks.
//
// THE ATTRIBUTE KEYS ARE pkg/authbus's, NOT ARBITRARY. principal.subject and
// principal.tenant are the two keys BusRecorder reads to stamp an envelope's
// tenant_id and partition key (pkg/authbus/recorder.go), so the day this service
// gains a bus.Producer, swapping the SlogRecorder for a BusRecorder is a wiring
// change and not a second mapping to keep in step.
//
// Every field comes from the VERIFIED TOKEN or from the loaded account. None is
// taken from the request body, which is #444's rule: an audit field a caller can
// set is not an audit field.
func statusDecisionLog(
	claims *identity.Claims, u *identity.User, status identity.Status, at time.Time,
) *observationpb.DecisionLog {
	action := "enable"
	if status == identity.StatusDisabled {
		action = "disable"
	}
	return &observationpb.DecisionLog{
		// "{type}:{id}", the DecisionLog.decider convention. The deciding party is
		// the named operator, not this service: the service is only what carried it.
		Decider: "operator:" + claims.Subject,
		Summary: fmt.Sprintf("%s account %s in tenant %s, by %s",
			strings.ToUpper(action), u.Subject, u.Tenant, claims.Subject),
		Attributes: map[string]string{
			"action":            "identity.account." + action,
			"account.subject":   u.Subject,
			"account.tenant":    u.Tenant,
			"account.status":    string(status),
			"principal.subject": claims.Subject,
			"principal.tenant":  claims.Tenant,
			"occurred_at":       at.Format(time.RFC3339Nano),
		},
	}
}
