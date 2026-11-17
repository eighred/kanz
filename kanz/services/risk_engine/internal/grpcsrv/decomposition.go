package grpcsrv

import (
	"context"

	domainpb "github.com/eighred/kanz/kanz-schemas-go/domain/v1"
	querypb "github.com/eighred/kanz/kanz-schemas-go/query/v1"

	v1 "github.com/eighred/kanz/internal/risk/api/v1"
	"github.com/eighred/kanz/internal/risk/publish"
)

func (s *Server) DecomposeRisk(ctx context.Context, req *querypb.DecomposeRiskRequest) (*querypb.DecomposeRiskResponse, error) {
	if err := requireDecimalDomain(req); err != nil {
		return nil, err
	}
	asOf, err := asOfTime(req.GetAsOf())
	if err != nil {
		return nil, err
	}
	resp, err := s.engine.DecomposeRisk(ctx, v1.DecomposeRiskRequest{PortfolioID: v1.PortfolioID(req.GetPortfolioId()), AsOf: asOf})
	if err != nil {
		return nil, mapError(err)
	}
	d := resp.Decomposition
	coverage := publish.ToProtoInputCoverage(d.Coverage)
	if coverage == nil {
		// Unlike a legacy measure that reports no coverage, this method has
		// assessed a known empty book against an actual model.
		coverage = &domainpb.InputCoverage{}
	}
	out := &querypb.DecomposeRiskResponse{
		PortfolioId: string(resp.PortfolioID), AsOf: nonZeroTimestamp(resp.AsOf), CurrencyCode: resp.CurrencyCode,
		Total: d.Total, Systematic: d.Systematic, Specific: d.Specific, SpecificComponent: d.SpecificComponent,
		Coverage: coverage, Provenance: publish.ToProtoProvenance(d.Provenance),
		QualityFlags: protoFlags(resp.QualityFlags), OwnerTenant: s.ownerTenant, SourcePosition: resp.SourcePosition,
	}
	for _, f := range d.ByFactor {
		out.ByFactor = append(out.ByFactor, &querypb.FactorRiskContribution{Key: f.Key, Exposure: f.Exposure, Component: f.Component})
	}
	for _, i := range d.ByInstrument {
		out.ByInstrument = append(out.ByInstrument, &querypb.InstrumentRiskContribution{InstrumentId: string(i.InstrumentID), Marginal: i.Marginal, Component: i.Component})
	}
	return out, nil
}
