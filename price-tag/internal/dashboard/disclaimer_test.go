package dashboard

import (
	"bytes"
	"testing"
)

// The usage pages show what this EnMaaS deployment records. Keep the scope
// note visible so users do not read the numbers as a complete view of AI spend
// across other tools or platforms.
func TestUsagePagesStateEnMaaSScope(t *testing.T) {
	for _, tc := range []struct {
		file    string
		want    string
		notWant []string
	}{
		{
			file: "account.html",
			want: "This page shows the usage recorded by this EnMaaS deployment for you.",
			notWant: []string{
				"went through the EnMaaS gateway",
				"EnMaaS traffic only.",
			},
		},
		{
			file: "manager.html",
			want: "This page shows the usage recorded by this EnMaaS deployment for the people in this view.",
			notWant: []string{
				"for your team",
				"EnMaaS traffic only.",
			},
		},
	} {
		data, err := FS.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte(tc.want)) {
			t.Errorf("%s: scope disclaimer missing %q", tc.file, tc.want)
		}
		if !bytes.Contains(data, []byte("EnMaaS usage.")) {
			t.Errorf("%s: scope disclaimer heading missing", tc.file)
		}
		for _, notWant := range tc.notWant {
			if bytes.Contains(data, []byte(notWant)) {
				t.Errorf("%s: scope disclaimer contains stale wording %q", tc.file, notWant)
			}
		}
	}
}
