package main

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "mTLS requested but no workload socket is rejected",
			cfg:     Config{Plaintext: false, SPIFFESocket: ""},
			wantErr: true,
		},
		{
			name:    "mTLS requested with a workload socket is accepted",
			cfg:     Config{Plaintext: false, SPIFFESocket: "/some/sock"},
			wantErr: false,
		},
		{
			name:    "plaintext dev rig needs no socket",
			cfg:     Config{Plaintext: true, SPIFFESocket: ""},
			wantErr: false,
		},
		{
			name:    "empty MetricsURL is a supported bus-plus-health-only mode",
			cfg:     Config{Plaintext: true, MetricsURL: ""},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("validate(%+v) = nil, want error", tc.cfg)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validate(%+v) = %v, want nil", tc.cfg, err)
			}
		})
	}
}
