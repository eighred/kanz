package optimization

import "testing"

func TestPSDRank(t *testing.T) {
	tests := []struct {
		name    string
		cov     [][]float64
		wantErr bool
		// wantRank is the rank derived by hand from the matrix, not read off
		// psdRank: an all-zero row/column or an exactly-collinear pair removes one
		// independent direction each.
		wantRank int
	}{
		{"positive-definite diagonal", diag(0.04, 0.09), false, 2},
		{"positive-definite correlated", [][]float64{{0.04, 0.036}, {0.036, 0.04}}, false, 2},
		{"singular PSD (perfectly correlated)", [][]float64{{1, 1}, {1, 1}}, false, 1},
		{"indefinite (positive diagonal)", [][]float64{{1, -2}, {-2, 1}}, true, 0},
		{"asymmetric", [][]float64{{1, 0.5}, {0.4, 1}}, true, 0},
		{"1x1 non-negative", [][]float64{{0.04}}, false, 1},
		{"1x1 negative", [][]float64{{-0.01}}, true, 0},
		{"numerical-noise near-PSD accepted", [][]float64{{1, 1.0000000001}, {1.0000000001, 1}}, false, 1},
		// THE MATRIX THAT STARTED #621: every eigenvalue is 0, so the rank is 0 and
		// every portfolio it prices is riskless. It is still ACCEPTED — the caller
		// is told by the rank, not by a refusal.
		{"all zeros", [][]float64{{0, 0}, {0, 0}}, false, 0},
		{"one riskless asset among two", [][]float64{{0.04, 0}, {0, 0}}, false, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rank, err := psdRank(tc.cov)
			if tc.wantErr && err != ErrNotPSD {
				t.Fatalf("want ErrNotPSD, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if rank != tc.wantRank {
				t.Fatalf("rank: got %d want %d — the rank is what separates a covariance that "+
					"supports a risk number from one that only looks like it does (#621)", rank, tc.wantRank)
			}
		})
	}
}
