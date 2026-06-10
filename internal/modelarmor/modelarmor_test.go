package modelarmor

import (
	"strings"
	"testing"

	modelarmorpb "cloud.google.com/go/modelarmor/apiv1/modelarmorpb"
)

func TestLocationFromTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "well-formed resource name",
			input: "projects/gen-lang-client-0479899208/locations/us-central1/templates/sre-bible",
			want:  "us-central1",
		},
		{
			name:  "different location",
			input: "projects/p/locations/europe-west4/templates/t",
			want:  "europe-west4",
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:    "missing locations segment",
			input:   "projects/p/templates/t",
			wantErr: true,
		},
		{
			name:    "locations key present but no value",
			input:   "projects/p/locations",
			wantErr: true,
		},
		{
			name:    "empty location value",
			input:   "projects/p/locations//templates/t",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := locationFromTemplate(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("locationFromTemplate(%q): expected error, got nil (got %q)", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("locationFromTemplate(%q): unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("locationFromTemplate(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// piAndJailbreakResult builds a FilterResults entry for the prompt-injection /
// jailbreak filter with the given match state.
func piAndJailbreakResult(state modelarmorpb.FilterMatchState) *modelarmorpb.FilterResult {
	return &modelarmorpb.FilterResult{
		FilterResult: &modelarmorpb.FilterResult_PiAndJailbreakFilterResult{
			PiAndJailbreakFilterResult: &modelarmorpb.PiAndJailbreakFilterResult{
				MatchState: state,
			},
		},
	}
}

func TestInterpretVerdict(t *testing.T) {
	t.Parallel()

	t.Run("match found is blocked and names the matched filter", func(t *testing.T) {
		t.Parallel()
		res := &modelarmorpb.SanitizationResult{
			FilterMatchState: modelarmorpb.FilterMatchState_MATCH_FOUND,
			FilterResults: map[string]*modelarmorpb.FilterResult{
				"pi_and_jailbreak": piAndJailbreakResult(modelarmorpb.FilterMatchState_MATCH_FOUND),
			},
		}
		blocked, reason := interpretVerdict(res)
		if !blocked {
			t.Error("MATCH_FOUND must be blocked, got blocked=false")
		}
		if !strings.Contains(reason, "pi_and_jailbreak") {
			t.Errorf("reason should name the matched filter, got %q", reason)
		}
	})

	t.Run("no match found is not blocked", func(t *testing.T) {
		t.Parallel()
		res := &modelarmorpb.SanitizationResult{
			FilterMatchState: modelarmorpb.FilterMatchState_NO_MATCH_FOUND,
			FilterResults: map[string]*modelarmorpb.FilterResult{
				"pi_and_jailbreak": piAndJailbreakResult(modelarmorpb.FilterMatchState_NO_MATCH_FOUND),
			},
		}
		blocked, reason := interpretVerdict(res)
		if blocked {
			t.Error("NO_MATCH_FOUND must not be blocked, got blocked=true")
		}
		if reason != "" {
			t.Errorf("reason should be empty when nothing matched, got %q", reason)
		}
	})

	t.Run("nil result is not blocked", func(t *testing.T) {
		t.Parallel()
		blocked, reason := interpretVerdict(nil)
		if blocked {
			t.Error("nil result must not be blocked")
		}
		if reason != "" {
			t.Errorf("reason should be empty for nil result, got %q", reason)
		}
	})

	t.Run("multiple matched filters are all named", func(t *testing.T) {
		t.Parallel()
		res := &modelarmorpb.SanitizationResult{
			FilterMatchState: modelarmorpb.FilterMatchState_MATCH_FOUND,
			FilterResults: map[string]*modelarmorpb.FilterResult{
				"pi_and_jailbreak": piAndJailbreakResult(modelarmorpb.FilterMatchState_MATCH_FOUND),
				"rai": {
					FilterResult: &modelarmorpb.FilterResult_RaiFilterResult{
						RaiFilterResult: &modelarmorpb.RaiFilterResult{
							MatchState: modelarmorpb.FilterMatchState_MATCH_FOUND,
						},
					},
				},
			},
		}
		blocked, reason := interpretVerdict(res)
		if !blocked {
			t.Fatal("MATCH_FOUND must be blocked")
		}
		if !strings.Contains(reason, "pi_and_jailbreak") || !strings.Contains(reason, "rai") {
			t.Errorf("reason should name both matched filters, got %q", reason)
		}
	})
}

// TestInterpretVerdict_OverallStateAuthoritative documents that the block decision
// follows the overall FilterMatchState, NOT the per-filter results (which only feed
// the log reason). A partial config could leave an individual filter MATCH_FOUND
// while the overall state is NO_MATCH_FOUND; the gate must defer to the overall
// state and NOT block.
func TestInterpretVerdict_OverallStateAuthoritative(t *testing.T) {
	t.Parallel()

	res := &modelarmorpb.SanitizationResult{
		FilterMatchState: modelarmorpb.FilterMatchState_NO_MATCH_FOUND,
		FilterResults: map[string]*modelarmorpb.FilterResult{
			"pi_and_jailbreak": piAndJailbreakResult(modelarmorpb.FilterMatchState_MATCH_FOUND),
		},
	}
	blocked, reason := interpretVerdict(res)
	if blocked {
		t.Error("blocked must follow the overall FilterMatchState (NO_MATCH_FOUND), not a per-filter match")
	}
	// reason still names the individually-matched filter (logs only).
	if !strings.Contains(reason, "pi_and_jailbreak") {
		t.Errorf("reason should still name the per-filter match, got %q", reason)
	}
}

// matched is a shorthand for the MATCH_FOUND state used in fixture builders below.
const matched = modelarmorpb.FilterMatchState_MATCH_FOUND

// TestInterpretVerdict_FilterVariants covers every per-filter oneof variant that
// filterResultMatched inspects, ensuring each contributes its name to the reason
// and is recognised as a match. Overall state is MATCH_FOUND for each case.
func TestInterpretVerdict_FilterVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		key    string
		result *modelarmorpb.FilterResult
	}{
		{
			name: "rai",
			key:  "rai",
			result: &modelarmorpb.FilterResult{
				FilterResult: &modelarmorpb.FilterResult_RaiFilterResult{
					RaiFilterResult: &modelarmorpb.RaiFilterResult{MatchState: matched},
				},
			},
		},
		{
			name: "malicious_uris",
			key:  "malicious_uris",
			result: &modelarmorpb.FilterResult{
				FilterResult: &modelarmorpb.FilterResult_MaliciousUriFilterResult{
					MaliciousUriFilterResult: &modelarmorpb.MaliciousUriFilterResult{MatchState: matched},
				},
			},
		},
		{
			name: "csam",
			key:  "csam",
			result: &modelarmorpb.FilterResult{
				FilterResult: &modelarmorpb.FilterResult_CsamFilterFilterResult{
					CsamFilterFilterResult: &modelarmorpb.CsamFilterResult{MatchState: matched},
				},
			},
		},
		{
			name: "virus_scan",
			key:  "virus_scan",
			result: &modelarmorpb.FilterResult{
				FilterResult: &modelarmorpb.FilterResult_VirusScanFilterResult{
					VirusScanFilterResult: &modelarmorpb.VirusScanFilterResult{MatchState: matched},
				},
			},
		},
		{
			name: "sdp via inspect result",
			key:  "sdp",
			result: &modelarmorpb.FilterResult{
				FilterResult: &modelarmorpb.FilterResult_SdpFilterResult{
					SdpFilterResult: &modelarmorpb.SdpFilterResult{
						Result: &modelarmorpb.SdpFilterResult_InspectResult{
							InspectResult: &modelarmorpb.SdpInspectResult{MatchState: matched},
						},
					},
				},
			},
		},
		{
			name: "sdp via deidentify result",
			key:  "sdp",
			result: &modelarmorpb.FilterResult{
				FilterResult: &modelarmorpb.FilterResult_SdpFilterResult{
					SdpFilterResult: &modelarmorpb.SdpFilterResult{
						Result: &modelarmorpb.SdpFilterResult_DeidentifyResult{
							DeidentifyResult: &modelarmorpb.SdpDeidentifyResult{MatchState: matched},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := &modelarmorpb.SanitizationResult{
				FilterMatchState: modelarmorpb.FilterMatchState_MATCH_FOUND,
				FilterResults:    map[string]*modelarmorpb.FilterResult{tc.key: tc.result},
			}
			blocked, reason := interpretVerdict(res)
			if !blocked {
				t.Errorf("%s: MATCH_FOUND must be blocked", tc.name)
			}
			if !strings.Contains(reason, tc.key) {
				t.Errorf("%s: reason should name %q, got %q", tc.name, tc.key, reason)
			}
		})
	}
}

func TestRegionalEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		location string
		want     string
	}{
		{"us-central1", "modelarmor.us-central1.rep.googleapis.com:443"},
		{"europe-west4", "modelarmor.europe-west4.rep.googleapis.com:443"},
	}
	for _, tc := range tests {
		if got := regionalEndpoint(tc.location); got != tc.want {
			t.Errorf("regionalEndpoint(%q) = %q, want %q", tc.location, got, tc.want)
		}
	}
}
