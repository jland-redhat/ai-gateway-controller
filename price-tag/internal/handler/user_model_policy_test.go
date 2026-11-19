package handler

import "testing"

func TestParsePartnerUserIDPath(t *testing.T) {
	const prefix = "/api/v1/model-policies/users/"
	const userID = "123e4567-e89b-12d3-a456-426614174000"
	tests := []struct {
		name    string
		path    string
		want    string
		wantErr bool
	}{
		{name: "UUID and documented suffix", path: prefix + userID + "/allowlist", want: userID},
		{name: "missing suffix", path: prefix + "alice", wantErr: true},
		{name: "empty username", path: prefix + "/allowlist", wantErr: true},
		{name: "multiple path segments", path: prefix + "alice/other/allowlist", wantErr: true},
		{name: "encoded slash", path: prefix + "alice%2Fother/allowlist", wantErr: true},
		{name: "wrong endpoint", path: "/api/v1/usage/users/alice/allowlist", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePartnerUserIDPath(tt.path, prefix)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parsePartnerUserIDPath(%q) error = %v, wantErr %v", tt.path, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("parsePartnerUserIDPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
