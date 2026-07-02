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

// CRIF RiskType → FRTB risk class, delta and vega families. Unmapped types
// (margin-only rows like Risk_XCcyBasis, or DRC/RRAO rows handled elsewhere)
// are skipped by the mappers.
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
)

// DeltaSensitivities maps the CRIF's delta rows into SBM sensitivities: risk
// class per RiskType, factor identified by Qualifier + tenor/name labels.
func DeltaSensitivities(recs []CRIFRecord) []Sensitivity {
	return mapSensitivities(recs, crifDeltaClass)
}

// VegaSensitivities maps the CRIF's vega rows into SBM sensitivities (the
// vega charge runs the same aggregation under its own params).
func VegaSensitivities(recs []CRIFRecord) []Sensitivity {
	return mapSensitivities(recs, crifVegaClass)
}

func mapSensitivities(recs []CRIFRecord, classOf map[string]string) []Sensitivity {
	var out []Sensitivity
	for _, r := range recs {
		class, ok := classOf[r.RiskType]
		if !ok {
			continue
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
	return out
}
