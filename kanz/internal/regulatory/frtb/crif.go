package frtb

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Live CRIF ingestion (PARITY-03g). The Common Risk Interchange Format is the
// industry wire shape for sensitivity data — tab- or semicolon-separated rows
// with a header naming at least RiskType, Qualifier, Bucket, Label1, Label2,
// Amount. ParseCRIF reads the file shape; the mappers below classify records
// into the delta / vega sensitivity sets the SBM consumes, so a live CRIF
// export drives the capital engines without hand-massaging.

// CRIFRecord is one parsed CRIF row.
type CRIFRecord struct {
	RiskType  string
	Qualifier string
	Bucket    string
	Label1    string
	Label2    string
	Amount    float64
}

// ErrCRIF is returned for a malformed CRIF stream.
var ErrCRIF = errors.New("frtb: malformed CRIF")

// ParseCRIF reads a CRIF export: a header line naming the columns, then one
// record per line. Separator is auto-detected (tab or semicolon). Unknown
// columns are ignored; RiskType and Amount are required.
func ParseCRIF(r io.Reader) ([]CRIFRecord, error) {
	sc := bufio.NewScanner(r)
	if !sc.Scan() {
		return nil, fmt.Errorf("%w: empty stream", ErrCRIF)
	}
	header := sc.Text()
	sep := "\t"
	if !strings.Contains(header, "\t") && strings.Contains(header, ";") {
		sep = ";"
	}
	col := map[string]int{}
	for i, name := range strings.Split(header, sep) {
		col[strings.TrimSpace(name)] = i
	}
	riskTypeCol, ok := col["RiskType"]
	if !ok {
		return nil, fmt.Errorf("%w: header missing RiskType", ErrCRIF)
	}
	amountCol, ok := col["Amount"]
	if !ok {
		return nil, fmt.Errorf("%w: header missing Amount", ErrCRIF)
	}
	get := func(fields []string, name string) string {
		if i, ok := col[name]; ok && i < len(fields) {
			return strings.TrimSpace(fields[i])
		}
		return ""
	}

	var out []CRIFRecord
	line := 1
	for sc.Scan() {
		line++
		raw := sc.Text()
		if strings.TrimSpace(raw) == "" {
			continue
		}
		fields := strings.Split(raw, sep)
		if riskTypeCol >= len(fields) || amountCol >= len(fields) {
			return nil, fmt.Errorf("%w: line %d has %d fields", ErrCRIF, line, len(fields))
		}
		amt, err := strconv.ParseFloat(strings.TrimSpace(fields[amountCol]), 64)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d amount %q", ErrCRIF, line, fields[amountCol])
		}
		out = append(out, CRIFRecord{
			RiskType:  strings.TrimSpace(fields[riskTypeCol]),
			Qualifier: get(fields, "Qualifier"),
			Bucket:    get(fields, "Bucket"),
			Label1:    get(fields, "Label1"),
			Label2:    get(fields, "Label2"),
			Amount:    amt,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCRIF, err)
	}
	return out, nil
}

// CRIF RiskType -> FRTB risk class, delta and vega families.
//
// TOGETHER WITH crifHandledElsewhere THESE ARE THE KNOWN VOCABULARY, and a
// RiskType outside all three is an ERROR rather than a skipped row (#623). The
// doc here used to say unmapped types "are skipped by the mappers", which was a
// closed-world assumption about a VENDOR'S vocabulary that nothing enforced: a
// vendor adding a risk type, or a typo in an export, removed capital from a
// signed filing with no signal. See mapSensitivities.
var (
	crifDeltaClass = map[string]string{
		"Risk_IRCurve":    "GIRR",
		"Risk_Inflation":  "GIRR",
		"Risk_CreditQ":    "CSR",
		"Risk_CreditNonQ": "CSR",
		"Risk_Equity":     "Equity",
		"Risk_FX":         "FX",
		"Risk_Commodity":  "Commodity",
	}
	crifVegaClass = map[string]string{
		"Risk_IRVol":        "GIRR",
		"Risk_CreditVol":    "CSR",
		"Risk_EquityVol":    "Equity",
		"Risk_FXVol":        "FX",
		"Risk_CommodityVol": "Commodity",
	}

	// crifHandledElsewhere are RiskTypes this file KNOWS and deliberately does
	// not turn into SBM sensitivities, each with the reason.
	//
	// IT IS DELIBERATELY SHORT, AND SHORT IS THE HONEST LENGTH. It holds what
	// this repository can actually attest to. The DRC and RRAO charges are
	// computed elsewhere in this package, but nothing here has ever seen a real
	// CRIF export naming their RiskTypes, and guessing the vendor spelling of a
	// row in order to pre-approve dropping it would be the same closed-world
	// assumption this replaced, written down more confidently. The first live
	// CRIF feed adds them, one at a time, each as a decision somebody made.
	crifHandledElsewhere = map[string]string{
		"Risk_XCcyBasis": "cross-currency basis is a MARGIN-ONLY row (ISDA SIMM) and carries no " +
			"FRTB SBM charge; it is present in a CRIF export because the same file feeds margin.",
	}
)

// ErrUnknownRiskType is returned when a CRIF row names a RiskType that is in
// neither sensitivity family nor the handled-elsewhere list.
//
// IT IS AN ERROR AND NOT A SKIP, for ErrUncovered's reason one layer down (#565,
// #623): a dropped row and a book with no such risk produce the same smaller
// FRTB_TOTAL, and the second one gets signed. The failure is one-directional and
// in the filer's favour, which is the direction nobody reports.
var ErrUnknownRiskType = errors.New("frtb: CRIF row names a RiskType this mapping does not know")

// DeltaSensitivities maps the CRIF's delta rows into SBM sensitivities: risk
// class per RiskType, factor identified by Qualifier + tenor/name labels.
//
// It returns ErrUnknownRiskType if any row names a RiskType outside the known
// vocabulary — including rows this family does not itself emit, because a vega
// row is known to the CRIF even though only VegaSensitivities turns it into one.
func DeltaSensitivities(recs []CRIFRecord) ([]Sensitivity, error) {
	return mapSensitivities(recs, crifDeltaClass)
}

// VegaSensitivities maps the CRIF's vega rows into SBM sensitivities (the
// vega charge runs the same aggregation under its own params).
func VegaSensitivities(recs []CRIFRecord) ([]Sensitivity, error) {
	return mapSensitivities(recs, crifVegaClass)
}

// mapSensitivities turns the rows this family owns into sensitivities, skips the
// rows another family or another charge owns, and REFUSES anything it has never
// heard of.
//
// The three-way split is the whole repair. Before, the second and third cases
// were one `continue`, so "a vega row, correctly not a delta" and "a RiskType
// nobody has ever seen" were the same silent outcome (#623).
func mapSensitivities(recs []CRIFRecord, classOf map[string]string) ([]Sensitivity, error) {
	var out []Sensitivity
	for _, r := range recs {
		class, ok := classOf[r.RiskType]
		if !ok {
			if err := knownRiskType(r.RiskType); err != nil {
				return nil, err
			}
			continue // known, and owned by another family or another charge
		}
		factor := r.Qualifier
		if r.Label1 != "" {
			factor += "/" + r.Label1
		}
		if r.Label2 != "" {
			factor += "/" + r.Label2
		}
		out = append(out, Sensitivity{
			RiskClass: class,
			Bucket:    r.Bucket,
			Factor:    factor,
			Amount:    r.Amount,
		})
	}
	return out, nil
}

// knownRiskType reports whether the CRIF vocabulary covers riskType at all.
//
// The union of BOTH sensitivity families plus the handled-elsewhere list is the
// vocabulary — not the single family currently being mapped. Checking only the
// caller's map would make every vega row unknown to DeltaSensitivities and fail
// every real export.
func knownRiskType(riskType string) error {
	if _, ok := crifDeltaClass[riskType]; ok {
		return nil
	}
	if _, ok := crifVegaClass[riskType]; ok {
		return nil
	}
	if _, ok := crifHandledElsewhere[riskType]; ok {
		return nil
	}
	return fmt.Errorf("%w: %q is neither a delta nor a vega risk class here, and is not listed as "+
		"handled elsewhere. Dropping it would remove its capital from the filing silently. Map it "+
		"to a risk class, or add it to crifHandledElsewhere with the reason it carries no SBM "+
		"charge", ErrUnknownRiskType, riskType)
}
