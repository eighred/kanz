// Package volprofilefeed carries the intraday volume profile from the process
// that folds it to the process that schedules against it (#897).
//
// # The hole it fills
//
// #867 built the datum: internal/marketedge/volprofile, a per-(instrument,
// venue) intraday distribution of traded volume, folded on the market-data edge.
// #869 built the algorithms: VWAP and POV in internal/execution/algo, which
// schedule a parent order against that shape and REFUSE rather than degrade when
// it is UNKNOWN. #869 also built the binding, internal/execution/marketview.
//
// Nothing joined them, because they run in different processes. The fold is in
// market-ingest, which holds the venue sockets; the schedule is derived in the
// OMS, which holds the orders. So all three OMS entries into the algo package
// passed algo.UnknownMarket, and every volume-driven order was refused at
// admission under NO_VOLUME_PROFILE — the fail-closed direction, and a feature
// nobody could use.
//
// # WHY A FACT, AND NOT A QUERY
//
// CLAUDE.md's rule is "everything is event-driven; state changes flow as FACTs on
// the bus; nothing polls", and it is the right answer here for three reasons that
// are specific rather than stylistic.
//
// A PUBLISHED SHAPE SURVIVES A POD ROLL. volprofile.Store is in-memory,
// per-process, and starts empty; it answers VerdictAbsent until MinSessions
// completed sessions accumulate. An OMS that folded its own would refuse every
// VWAP order for DAYS after each deploy, and the refusal would be indistinguishable
// from one for an instrument nobody measures.
//
// A PUBLISHED SHAPE IS ONE SHAPE. services/oms/internal/order.authorizeChild
// re-derives a parent's schedule to decide whether an inbound child really is a
// slice of it, and compares the quantity as an EXACT RATIONAL. Two pods folding
// their own tapes derive children that differ in the last digit, so the driver's
// own child is refused as a forgery and the parent stops advancing while every
// screen shows it working.
//
// AND A PUBLISHED SHAPE IS ADDRESSABLE. Version names one immutable curve, so a
// parent order can record which one it was planned against and every later
// derivation resolves THAT one. A query has no such handle: "the profile for
// BTC-USDT on XBIT" means something different after every session boundary, so a
// schedule derived at 23:59 and re-derived at 00:01 would not be the same
// schedule.
//
// # WHY THE PRODUCER IS THE MARKET-DATA EDGE
//
// Because that is where the fold already is, and moving it would break both ends.
// market-ingest holds the venue subscriptions; a fold anywhere else is a fold of
// data that has already been through a bus, with a hole wherever the bus dropped
// something. And the OMS has no market data at all — giving it a live fold would
// hand it a socket, a retention policy and a per-pod curve, which is the second
// reason above arriving through the composition root instead of through a design
// document.
//
// # BOTH HALVES LIVE HERE, DELIBERATELY
//
// The producer's sweep and the consumer's registry are one package, following
// internal/venuemargin and internal/cashview. The wire mapping is stated once, so
// a field the producer sets and the consumer forgets to read cannot happen; and
// Version is computed by ONE function, which is what lets the consumer RECOMPUTE
// it and refuse a message whose version does not describe its own content. Two
// packages would be two copies of that function, and a pin that addressed the
// wrong curve is exactly the mislabelling the pin exists to prevent.
package volprofilefeed

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	commonpb "github.com/eighred/kanz/kanz-schemas-go/common/v1"
	marketpb "github.com/eighred/kanz/kanz-schemas-go/market/v1"

	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/marketedge/volprofile"
)

// Subject is where the market-data edge announces a series' intraday volume
// profile.
//
// ON market.crypto.*, NOT A NEW SUBJECT SPACE. market.crypto.trade,
// market.crypto.bar (#425) and market.crypto.ingestion_coverage (#591) already
// ride it: the MARKET stream carries market.> and the Kafka topic is derived
// from the first two tokens, so this needs no new stream, no new topic and no new
// archival decision. A second plane for one subject would be a stream, a topic,
// an archiver subscription and a DR restore set that all exist to be forgotten
// separately.
//
// DECLARED ONCE, and both the publisher and the consumer import this constant —
// the stance internal/venuemargin takes, and the reason it needs no arch guard
// holding two copies equal.
const Subject = "market.crypto.volume_profile"

// domain is the envelope domain every market.v1 FACT carries.
const domain = "market"

// ErrProfile refuses a message that cannot be trusted to describe a market.
var ErrProfile = errors.New("volprofilefeed: profile")

// Series identifies one profile's subject: one instrument on one venue.
//
// It is volprofile.Series' two fields, re-declared for the same reason that
// package re-declares bars.Series': taking the other package's type here would be
// harmless, but this one is the KEY OF A REGISTRY and a registry keyed by a type
// from a package it does not otherwise need is a dependency that outlives the
// convenience. TestSeriesMatchesTheFold holds the two equal.
type Series struct {
	InstrumentID string
	Venue        string
}

func (s Series) String() string { return s.InstrumentID + "@" + s.Venue }

// Profile is one published shape, decoded.
//
// IT IS A VALUE AND IT IS IMMUTABLE ONCE DECODED. The Expected slice is the
// caller's own copy, because *big.Rat is a pointer and a registry that handed out
// its own would let one reader's arithmetic silently rewrite the curve every
// later reader schedules against.
type Profile struct {
	Series  Series
	Version string
	AsOf    time.Time
	Bucket  time.Duration
	Horizon time.Duration
	Verdict volprofile.Verdict

	// Sessions, Oldest, Newest and the Window counts are reported in EVERY
	// verdict, including the refusals: an operator has to be able to tell
	// "nearly ready" from "nothing here", and a refusal that carries no counts
	// cannot.
	Sessions int
	Oldest   time.Time
	Newest   time.Time
	Window   volprofile.Window

	// Expected[i] is the quantity expected to trade in intraday bucket i of a
	// typical session. NIL UNLESS Verdict.Known, under exactly the rule
	// volprofile.Answer.Shares follows — a caller that reads it without checking
	// gets a nil slice rather than a flat curve, and the failure is loud instead
	// of a schedule that looks right.
	Expected []*big.Rat
}

// Known reports whether this profile carries a measured curve.
func (p Profile) Known() bool { return p.Verdict.Known() && len(p.Expected) > 0 }

// verdictToWire and verdictFromWire are the ONE mapping between the fold's
// vocabulary and the wire's.
//
// A SWITCH IN EACH DIRECTION, WITH NO DEFAULT THAT INVENTS A KNOWN. Every value
// this build does not recognise — including a numeric one from a newer producer —
// lands on the non-known side, so a rolling deploy refuses an order rather than
// scheduling it against a verdict it cannot read.
func verdictToWire(v volprofile.Verdict) marketpb.VolumeProfileVerdict {
	switch v {
	case volprofile.VerdictAbsent:
		return marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_ABSENT
	case volprofile.VerdictStale:
		return marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_STALE
	case volprofile.VerdictTooFewSessions:
		return marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_TOO_FEW_SESSIONS
	case volprofile.VerdictKnown:
		return marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_KNOWN
	default:
		return marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_UNSPECIFIED
	}
}

func verdictFromWire(v marketpb.VolumeProfileVerdict) volprofile.Verdict {
	switch v {
	case marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_STALE:
		return volprofile.VerdictStale
	case marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_TOO_FEW_SESSIONS:
		return volprofile.VerdictTooFewSessions
	case marketpb.VolumeProfileVerdict_VOLUME_PROFILE_VERDICT_KNOWN:
		return volprofile.VerdictKnown
	default:
		// ABSENT AND UNSPECIFIED COLLAPSE HERE, AND ONLY HERE. volprofile's zero
		// value is VerdictAbsent precisely so an answer nobody filled in fails
		// closed; a wire value this build cannot name gets the same treatment for
		// the same reason.
		return volprofile.VerdictAbsent
	}
}

// Encode maps a fold's answer onto the wire and stamps its version.
//
// # THE ROUNDING HAPPENS HERE, ONCE
//
// The fold's shape is a mean of per-session distributions, so it is generically a
// non-terminating rational; a decimal is what this estate transports. Rounding it
// in ONE place is what makes the schedule reproducible: every consumer reads the
// same coefficients and derives the same children, and the version below is
// computed over the ROUNDED values rather than over the originals, so the pin
// addresses exactly what a reader will see.
//
// dec.ToProtoScaled rather than dec.ToProto, because a session's volume on a
// liquid pair is a large number and ToProto wraps its int64 coefficient at around
// 9.2e10 at scale 8 — a wrapped coefficient is a fabricated market the scheduler
// would then size children against. ToProtoScaled raises the exponent instead, which
// keeps the magnitude right, and refuses only what cannot be expressed at all.
//
// A bucket that will not convert refuses the WHOLE profile rather than being
// dropped from it: a curve missing one bin is a curve that says nothing traded in
// it, which sizes every other slice wrongly and refuses any slice landing there.
func Encode(a volprofile.Answer, horizon time.Duration) (*marketpb.VolumeProfile, error) {
	if a.Series.InstrumentID == "" || a.Series.Venue == "" {
		return nil, fmt.Errorf("%w: a profile is keyed by (instrument, venue) and this answer "+
			"names %q — a reader could not tell which book it describes", ErrProfile, a.Series.String())
	}
	if a.AsOf.IsZero() {
		return nil, fmt.Errorf("%w: %s has no as-of, and a shape with no instant cannot be "+
			"ordered against the sessions behind it", ErrProfile, a.Series.String())
	}
	if a.Bucket <= 0 || volprofile.Session%a.Bucket != 0 {
		return nil, fmt.Errorf("%w: %s has bucket %s, which does not divide the %s session evenly",
			ErrProfile, a.Series.String(), a.Bucket, volprofile.Session)
	}
	if horizon <= 0 {
		return nil, fmt.Errorf("%w: %s carries no retention horizon, and a consumer would have to "+
			"invent the bound on how long an interval this curve may be integrated over",
			ErrProfile, a.Series.String())
	}

	pb := &marketpb.VolumeProfile{
		InstrumentId:   a.Series.InstrumentID,
		Mic:            a.Series.Venue,
		AsOf:           tsOf(a.AsOf),
		Session:        durOf(volprofile.Session),
		Bucket:         durOf(a.Bucket),
		Horizon:        durOf(horizon),
		Verdict:        verdictToWire(a.Verdict),
		Sessions:       uint32(max(a.Sessions, 0)),
		OldestSession:  tsOf(a.Oldest),
		NewestSession:  tsOf(a.Newest),
		WindowBuckets:  uint32(max(a.Window.Buckets, 0)),
		WindowObserved: uint32(max(a.Window.Observed, 0)),
	}

	if a.Known() {
		want := volprofile.BucketsPerSession(a.Bucket)
		if len(a.Shares) != want || a.SessionVolume == nil || a.SessionVolume.Sign() <= 0 {
			return nil, fmt.Errorf("%w: %s says KNOWN with %d of %d buckets and a level of %s — a "+
				"verdict asserting a shape that is not there would reach a scheduler as a market",
				ErrProfile, a.Series.String(), len(a.Shares), want, ratString(a.SessionVolume))
		}
		pb.ExpectedVolume = make([]*commonpb.Decimal, len(a.Shares))
		for i, sh := range a.Shares {
			if sh == nil || sh.Sign() < 0 {
				return nil, fmt.Errorf("%w: %s bucket %d carries %s", ErrProfile,
					a.Series.String(), i, ratString(sh))
			}
			d, ok := dec.ToProtoScaled(new(big.Rat).Mul(sh, a.SessionVolume))
			if !ok {
				return nil, fmt.Errorf("%w: %s bucket %d expects %s, which cannot be expressed as "+
					"a Decimal at any exponent — the profile is refused whole rather than published "+
					"with a hole that would read as a bucket nothing trades in", ErrProfile,
					a.Series.String(), i, new(big.Rat).Mul(sh, a.SessionVolume).FloatString(12))
			}
			pb.ExpectedVolume[i] = d
		}
	}

	pb.Version = versionOf(pb)
	return pb, nil
}

// Decode maps the wire back onto a Profile and REFUSES a message whose version
// does not describe its own content.
//
// # THE VERSION IS RECOMPUTED, NOT TRUSTED
//
// The version is what a parent order pins, so it is the handle every later
// derivation of that order's schedule resolves. A message carrying somebody
// else's version — a producer bug, a truncated republish, a hand-crafted
// message — would put a curve behind a pin that names a different one, and the
// two pods this whole design exists to keep in agreement would then agree on the
// version and disagree on the market. Recomputing costs one hash per message and
// removes the failure class.
//
// A NON-KNOWN VERDICT IS A VALID MESSAGE AND NOT AN ERROR. "Nobody has folded
// this series" and "folded, and there is not enough of it yet" are results an
// operator acts on, and publishing only the KNOWN profiles would collapse them
// into the same silence.
func Decode(pb *marketpb.VolumeProfile) (Profile, error) {
	if pb == nil {
		return Profile{}, fmt.Errorf("%w: no message", ErrProfile)
	}
	// THE DECIMAL DOMAIN IS BOUNDED BEFORE ANY CONVERSION. Decimal.exponent is an
	// unvalidated wire field, and dec.FromProto on a hostile one materialises
	// 10^exponent — the hang #95 records. The whole message is refused rather
	// than the bad bucket skipped, for Encode's reason: a curve with a hole reads
	// as a bucket nothing trades in.
	if path, ok := dec.InDomainDeep(pb); !ok {
		return Profile{}, fmt.Errorf("%w: %s@%s carries a Decimal outside the platform's exponent "+
			"domain at %s", ErrProfile, pb.GetInstrumentId(), pb.GetMic(), path)
	}
	if pb.GetInstrumentId() == "" || pb.GetMic() == "" {
		return Profile{}, fmt.Errorf("%w: names no (instrument, venue), so nothing could say which "+
			"book it describes", ErrProfile)
	}
	if got, want := pb.GetVersion(), versionOf(pb); got != want {
		return Profile{}, fmt.Errorf("%w: %s@%s carries version %q and its content hashes to %q — "+
			"a parent order pinning the first would resolve a curve nobody measured under that "+
			"name", ErrProfile, pb.GetInstrumentId(), pb.GetMic(), got, want)
	}

	bucket := pb.GetBucket().AsDuration()
	if bucket <= 0 || volprofile.Session%bucket != 0 {
		return Profile{}, fmt.Errorf("%w: %s@%s has bucket %s, which does not divide the %s session "+
			"evenly", ErrProfile, pb.GetInstrumentId(), pb.GetMic(), bucket, volprofile.Session)
	}
	if s := pb.GetSession().AsDuration(); s != volprofile.Session {
		// A PRODUCER ON A DIFFERENT SESSION LENGTH IS REFUSED, NOT RESCALED. The
		// integration in internal/execution/marketview truncates to
		// volprofile.Session to find a session boundary, so a curve built over a
		// different day would be folded onto the wrong hours — a wrong number
		// rather than a missing one.
		return Profile{}, fmt.Errorf("%w: %s@%s was built over a %s session and this build "+
			"integrates a %s one", ErrProfile, pb.GetInstrumentId(), pb.GetMic(), s, volprofile.Session)
	}
	horizon := pb.GetHorizon().AsDuration()
	if horizon <= 0 {
		return Profile{}, fmt.Errorf("%w: %s@%s carries no retention horizon", ErrProfile,
			pb.GetInstrumentId(), pb.GetMic())
	}

	out := Profile{
		Series:   Series{InstrumentID: pb.GetInstrumentId(), Venue: pb.GetMic()},
		Version:  pb.GetVersion(),
		AsOf:     pb.GetAsOf().AsTime().UTC(),
		Bucket:   bucket,
		Horizon:  horizon,
		Verdict:  verdictFromWire(pb.GetVerdict()),
		Sessions: int(pb.GetSessions()),
		Window: volprofile.Window{
			Buckets:  int(pb.GetWindowBuckets()),
			Observed: int(pb.GetWindowObserved()),
		},
	}
	if pb.GetOldestSession() != nil {
		out.Oldest = pb.GetOldestSession().AsTime().UTC()
	}
	if pb.GetNewestSession() != nil {
		out.Newest = pb.GetNewestSession().AsTime().UTC()
	}

	if !out.Verdict.Known() {
		if len(pb.GetExpectedVolume()) != 0 {
			return Profile{}, fmt.Errorf("%w: %s@%s says %s and carries a curve anyway — a reader "+
				"gating on one field and reading the other would schedule against it", ErrProfile,
				pb.GetInstrumentId(), pb.GetMic(), out.Verdict)
		}
		return out, nil
	}

	want := volprofile.BucketsPerSession(bucket)
	if len(pb.GetExpectedVolume()) != want {
		return Profile{}, fmt.Errorf("%w: %s@%s says KNOWN with %d of the %d buckets a %s bin needs "+
			"— the missing ones would read as intervals nothing trades in", ErrProfile,
			pb.GetInstrumentId(), pb.GetMic(), len(pb.GetExpectedVolume()), want, bucket)
	}
	out.Expected = make([]*big.Rat, want)
	for i, d := range pb.GetExpectedVolume() {
		r := dec.FromProto(d)
		if r.Sign() < 0 {
			return Profile{}, fmt.Errorf("%w: %s@%s bucket %d expects %s, and a negative quantity "+
				"is not a market", ErrProfile, pb.GetInstrumentId(), pb.GetMic(), i, r.RatString())
		}
		out.Expected[i] = r
	}
	return out, nil
}

// versionOf is the ONE version derivation, and it is a hash of the CONTENT.
//
// # Derived rather than allocated, and the difference is operational
//
// An allocated version — a counter, a uuid — makes a re-publish of an unchanged
// curve a NEW version, so a consumer's fold grows without bound and, worse, an
// order pinned before the republish and one pinned after are scheduled against
// the same market under two names. A content hash makes a republish idempotent at
// the key: the same sessions produce the same bytes produce the same version, on
// every replica and after every restart.
//
// EVERY FIELD THAT COULD CHANGE THE SCHEDULE IS IN IT. The curve obviously, but
// also the bucket width and the horizon — both are inputs to the integration in
// internal/execution/marketview — and the verdict, so a series that goes from
// TOO_FEW_SESSIONS to KNOWN with the same counts is a different version. The
// session bounds are in it too, so a curve rebuilt over a different set of days
// is a different version even when the arithmetic lands on the same numbers.
//
// # THE as-of IS DELIBERATELY NOT IN IT
//
// It is when the producer last ASSERTED the curve, not part of what the curve is,
// and including it would defeat the whole idea twice over. Every sweep would
// produce a fresh version of an unchanged shape, so the bus would carry hundreds
// of copies a day and a consumer's retained versions would fill with them and
// evict the pins still in use. And two replicas sweeping a second apart would
// publish two versions of one curve, so a pin taken against one would be
// unresolvable on the other — exactly the divergence the pin exists to remove.
//
// THE CONSEQUENCE IS STATED: a tampered as-of is not caught by the version check,
// because it does not change the curve. What it can change is ORDERING in the
// consumer's retained list, and therefore which version Registry.Current returns
// — a choice that is only ever made at admission, and only among curves that were
// all genuinely published. It cannot make a pinned derivation resolve a different
// shape, which is the property the recomputation is bought for.
//
// It is written field by field with a separator rather than over the marshalled
// message, because proto serialisation is not canonical: two builds may order or
// pack fields differently and the pin must not depend on which one published.
func versionOf(pb *marketpb.VolumeProfile) string {
	h := sha256.New()
	w := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0x1f})
		}
	}
	w("volprofile.v1")
	w(pb.GetInstrumentId(), pb.GetMic())
	w(strconv.FormatInt(int64(pb.GetSession().AsDuration()), 10))
	w(strconv.FormatInt(int64(pb.GetBucket().AsDuration()), 10))
	w(strconv.FormatInt(int64(pb.GetHorizon().AsDuration()), 10))
	w(strconv.Itoa(int(pb.GetVerdict())))
	w(strconv.FormatUint(uint64(pb.GetSessions()), 10))
	w(strconv.FormatInt(pb.GetOldestSession().AsTime().UTC().UnixNano(), 10))
	w(strconv.FormatInt(pb.GetNewestSession().AsTime().UTC().UnixNano(), 10))
	w(strconv.FormatUint(uint64(pb.GetWindowBuckets()), 10))
	w(strconv.FormatUint(uint64(pb.GetWindowObserved()), 10))
	w(strconv.Itoa(len(pb.GetExpectedVolume())))
	for _, d := range pb.GetExpectedVolume() {
		w(strconv.FormatInt(d.GetCoefficient(), 10), strconv.FormatInt(int64(d.GetExponent()), 10))
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func ratString(r *big.Rat) string {
	if r == nil {
		return "nothing"
	}
	return r.RatString()
}
