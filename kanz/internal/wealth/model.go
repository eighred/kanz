package wealth

import (
	"errors"
	"fmt"
	"github.com/eighred/kanz/internal/dec"
	"github.com/eighred/kanz/internal/optimization"
	"math/big"
	"sort"
	"strings"
	"time"
)

type RiskProfile int

const (
	// ProfileUnspecified is the zero value; never select on it.
	ProfileUnspecified RiskProfile = iota
	ProfileConservative
	ProfileModerate
	ProfileBalanced
	ProfileGrowth
	ProfileAggressive
)

// String names the profile for a log line, an operator-facing JSON field and an
// error message. It exists because the alternative everywhere is the raw ordinal:
// "profile=4" in a warning about a household nobody is checking is a number an
// operator has to go look up, and a read surface that returns 0 for
// ProfileUnspecified is indistinguishable from one that omitted the field.
func (p RiskProfile) String() string {
	switch p {
	case ProfileConservative:
		return "CONSERVATIVE"
	case ProfileModerate:
		return "MODERATE"
	case ProfileBalanced:
		return "BALANCED"
	case ProfileGrowth:
		return "GROWTH"
	case ProfileAggressive:
		return "AGGRESSIVE"
	case ProfileUnspecified:
		return "UNSPECIFIED"
	default:
		// Named rather than rendered blank: a profile this build does not know
		// reaching a log line is itself the finding.
		return fmt.Sprintf("RiskProfile(%d)", int(p))
	}
}

type ModelPortfolio struct {
	ModelID         string
	Profile         RiskProfile
	Targets         map[string]Number
	Tolerance       Number
	RecordedBy      string
	Reason          string
	LegacyPrecision bool
}

var ErrLegacyPrecision = errors.New("wealth: legacy precision is unverified; exact source replay required")

func (m ModelPortfolio) Clone() ModelPortfolio {
	targets := map[string]Number{}
	for k, v := range m.Targets {
		targets[k] = v
	}
	m.Targets = targets
	return m
}
func (m ModelPortfolio) Validate() error {
	if m.LegacyPrecision {
		return ErrLegacyPrecision
	}
	if strings.TrimSpace(m.ModelID) == "" || len(m.ModelID) > 256 {
		return errors.New("model_id is required and bounded")
	}
	if m.Profile < ProfileConservative || m.Profile > ProfileAggressive {
		return errors.New("risk_profile is required and must be known")
	}
	if len(m.Targets) == 0 || len(m.Targets) > 4096 {
		return errors.New("target_weights is required and bounded")
	}
	if strings.TrimSpace(m.RecordedBy) == "" {
		return errors.New("recorded_by is required")
	}
	if strings.TrimSpace(m.Reason) == "" {
		return errors.New("reason is required")
	}
	tolerance, err := m.Tolerance.Rat()
	if err != nil || tolerance.Sign() <= 0 || tolerance.Cmp(big.NewRat(1, 1)) > 0 {
		return errors.New("drift_tolerance must be > 0 and <= 1, exactly")
	}
	sum := new(big.Rat)
	for _, id := range m.TargetInstruments() {
		w := m.Targets[id]
		r, err := w.Rat()
		if err != nil || id == "" || len(id) > 256 || r.Sign() < 0 || r.Cmp(big.NewRat(1, 1)) > 0 {
			return errors.New("target_weights must be bounded exact weights between 0 and 1")
		}
		sum.Add(sum, r)
		if _, err = dec.ExactFromRat(sum); err != nil {
			return err
		}
	}
	if sum.Cmp(big.NewRat(1, 1)) != 0 {
		return errors.New("target_weights must sum to exactly 1")
	}
	return nil
}
func SelectModel(profile RiskProfile, models []ModelPortfolio) (ModelPortfolio, bool) {
	var selected ModelPortfolio
	found := false
	for _, m := range models {
		if m.Profile == profile {
			if found || m.Validate() != nil {
				return ModelPortfolio{}, false
			}
			selected = m.Clone()
			found = true
		}
	}
	return selected, found
}

type Drift struct {
	ByInstrument map[string]Number
	Max          Number
	Total        Number
}

func ComputeDrift(actual map[string]Number, model ModelPortfolio) (Drift, error) {
	if err := model.Validate(); err != nil {
		return Drift{}, err
	}
	if len(actual) > 4096 {
		return Drift{}, ErrValuation
	}
	ids := map[string]bool{}
	for id := range actual {
		ids[id] = true
	}
	for id := range model.Targets {
		ids[id] = true
	}
	d := Drift{ByInstrument: map[string]Number{}}
	max, total := new(big.Rat), new(big.Rat)
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		av, tv := Number("0"), Number("0")
		if v, ok := actual[id]; ok {
			av = v
		}
		if v, ok := model.Targets[id]; ok {
			tv = v
		}
		a, err := av.Rat()
		if err != nil {
			return Drift{}, err
		}
		t, err := tv.Rat()
		if err != nil {
			return Drift{}, err
		}
		delta := new(big.Rat).Sub(a, t)
		d.ByInstrument[id], err = dec.ExactFromRat(delta)
		if err != nil {
			return Drift{}, err
		}
		magnitude := new(big.Rat).Abs(delta)
		if magnitude.Cmp(max) > 0 {
			max.Set(magnitude)
		}
		total.Add(total, magnitude)
		if _, err = dec.ExactFromRat(total); err != nil {
			return Drift{}, err
		}
	}
	var err error
	d.Max, err = dec.ExactFromRat(max)
	if err != nil {
		return Drift{}, err
	}
	d.Total, err = dec.ExactFromRat(total)
	return d, err
}
func (d Drift) Breached(tolerance Number) (bool, error) {
	max, err := d.Max.Rat()
	if err != nil {
		return false, err
	}
	band, err := tolerance.Rat()
	if err != nil || band.Sign() <= 0 {
		return false, ErrValuation
	}
	return max.Cmp(band) > 0, nil
}

type Proposal struct {
	HouseholdID string
	ModelID     string
	Profile     RiskProfile
	Drift       Drift
	Rebalance   optimization.ExactRebalanceProposal
}

func Propose(profile RiskProfile, models []ModelPortfolio, vp VirtualPortfolio, prices map[string]Number, threshold Number, asOf time.Time) (Proposal, error) {
	model, ok := SelectModel(profile, models)
	if !ok {
		return Proposal{}, ErrNoModelForProfile
	}
	current, err := vp.Weights()
	if err != nil {
		return Proposal{}, err
	}
	d, err := ComputeDrift(current, model)
	if err != nil {
		return Proposal{}, err
	}
	rb, err := optimization.RebalanceExact(vp.HouseholdID, current, model.Targets, vp.TotalValue, prices, threshold, asOf)
	if err != nil {
		return Proposal{}, err
	}
	return Proposal{HouseholdID: vp.HouseholdID, ModelID: model.ModelID, Profile: profile, Drift: d, Rebalance: rb}, nil
}
func (m ModelPortfolio) TargetInstruments() []string {
	out := make([]string, 0, len(m.Targets))
	for id := range m.Targets {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
