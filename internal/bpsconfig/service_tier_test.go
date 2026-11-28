package bpsconfig

import (
	"fmt"
	"strings"
	"testing"
)

func TestServiceTierValidate(t *testing.T) {
	maximumIDs := make([]string, 256)
	for i := range maximumIDs {
		maximumIDs[i] = fmt.Sprintf("credential-%d.json", i)
	}
	for _, tc := range []struct {
		name  string
		ids   []string
		valid bool
	}{
		{"nil", nil, true},
		{"empty_list", []string{}, true},
		{"exact_ids", []string{"credential-id", "bps-account.json"}, true},
		{"case_sensitive", []string{"ID", "id"}, true},
		{"maximum_count", maximumIDs, true},
		{"excess_count", append(append([]string(nil), maximumIDs...), "overflow"), false},
		{"maximum_length", []string{strings.Repeat("x", 1024)}, true},
		{"excess_length", []string{strings.Repeat("x", 1025)}, false},
		{"empty_id", []string{""}, false},
		{"duplicate", []string{"same", "same"}, false},
		{"leading_space", []string{" account"}, false},
		{"trailing_space", []string{"account "}, false},
		{"whitespace", []string{"\t"}, false},
		{"newline", []string{"acc\nount"}, false},
		{"carriage_return", []string{"acc\rount"}, false},
		{"nul", []string{"acc\x00ount"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, global := range []bool{false, true} {
				err := (ServiceTier{ForceUltraFast: global, AuthIDs: tc.ids}).Validate()
				if (err == nil) != tc.valid {
					t.Fatalf("global=%t: valid=%t, error=%v", global, tc.valid, err)
				}
			}
		})
	}
}

func TestServiceTierForced(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy ServiceTier
		ids    []string
		want   bool
	}{
		{"off", ServiceTier{}, []string{"id", "file.json"}, false},
		{"global", ServiceTier{ForceUltraFast: true}, []string{"id"}, true},
		{"global_without_identity", ServiceTier{ForceUltraFast: true}, nil, true},
		{"selected_id", ServiceTier{AuthIDs: []string{"id"}}, []string{"id", "file.json"}, true},
		{"selected_filename", ServiceTier{AuthIDs: []string{"file.json"}}, []string{"id", "file.json"}, true},
		{"unselected", ServiceTier{AuthIDs: []string{"other"}}, []string{"id", "file.json"}, false},
		{"no_substring_matching", ServiceTier{AuthIDs: []string{"file"}}, []string{"file.json"}, false},
		{"case_sensitive", ServiceTier{AuthIDs: []string{"ID"}}, []string{"id"}, false},
		{"empty_identity", ServiceTier{AuthIDs: []string{""}}, []string{""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Forced(tc.ids...); got != tc.want {
				t.Fatalf("Forced()=%t, want %t", got, tc.want)
			}
		})
	}
}
