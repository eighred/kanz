package optimization

import "testing"

func TestCheckPSD(t *testing.T) {
	tests := []struct {
		name    string
		cov     [][]float64
		wantErr bool
	}{
		{"positive-definite diagonal", diag(0.04, 0.09), false},
		{"positive-definite correlated", [][]float64{{0.04, 0.036}, {0.036, 0.04}}, false},
		{"singular PSD (perfectly correlated)", [][]float64{{1, 1}, {1, 1}}, false},
		{"indefinite (positive diagonal)", [][]float64{{1, -2}, {-2, 1}}, true},
		{"asymmetric", [][]float64{{1, 0.5}, {0.4, 1}}, true},
		{"1x1 non-negative", [][]float64{{0.04}}, false},
		{"1x1 negative", [][]float64{{-0.01}}, true},
		{"numerical-noise near-PSD accepted", [][]float64{{1, 1.0000000001}, {1.0000000001, 1}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkPSD(tc.cov)
			if tc.wantErr && err != ErrNotPSD {
				t.Fatalf("want ErrNotPSD, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
