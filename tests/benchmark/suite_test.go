package benchmark

import (
	"strings"
	"testing"
)

func TestDefaultSuite_Valid(t *testing.T) {
	s := DefaultSuite()
	if err := s.Validate(); err != nil {
		t.Fatalf("DefaultSuite.Validate: %v", err)
	}
	if len(s.Scenarios) < 3 {
		t.Fatalf("DefaultSuite: want >=3 scenarios, got %d", len(s.Scenarios))
	}
}

func TestDefaultSuite_CoversRequiredMetrics(t *testing.T) {
	needed := map[Metric]bool{
		MetricPutP50:                  false,
		MetricPutP95:                  false,
		MetricPutP99:                  false,
		MetricGetP50:                  false,
		MetricGetP95:                  false,
		MetricGetP99:                  false,
		MetricCacheHitRatioHot:        false,
		MetricWasabiOriginEgressRatio: false,
		MetricListP95:                 false,
	}
	for _, sc := range DefaultSuite().Scenarios {
		for _, tg := range sc.Targets {
			needed[tg.Metric] = true
		}
	}
	for m, seen := range needed {
		if !seen {
			t.Errorf("DefaultSuite missing a Target for metric %q", m)
		}
	}
}

func TestDefaultSuite_ListSizes(t *testing.T) {
	wantSizes := map[int64]bool{
		ListSize10M:  false,
		ListSize100M: false,
		ListSize1B:   false,
	}
	for _, sc := range DefaultSuite().Scenarios {
		if sc.Workload.ListObjectCount == 0 {
			continue
		}
		if _, ok := wantSizes[sc.Workload.ListObjectCount]; !ok {
			t.Errorf("unexpected ListObjectCount %d in scenario %q", sc.Workload.ListObjectCount, sc.Name)
			continue
		}
		wantSizes[sc.Workload.ListObjectCount] = true
	}
	for size, ok := range wantSizes {
		if !ok {
			t.Errorf("DefaultSuite missing LIST scenario at size %d", size)
		}
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		suite   Suite
		wantSub string
	}{
		{
			name:    "empty",
			suite:   Suite{},
			wantSub: "suite name is required",
		},
		{
			name:    "no scenarios",
			suite:   Suite{Name: "s"},
			wantSub: "no scenarios",
		},
		{
			name: "scenario missing targets",
			suite: Suite{
				Name: "s",
				Scenarios: []Scenario{{
					Name: "sc",
					Workload: Workload{
						RequestMix:      map[string]float64{"PUT": 1.0},
						DurationSeconds: 1,
						TargetRPS:       1,
					},
				}},
			},
			wantSub: "at least one target",
		},
		{
			name: "mix not sum to 1",
			suite: Suite{
				Name: "s",
				Scenarios: []Scenario{{
					Name: "sc",
					Workload: Workload{
						RequestMix:      map[string]float64{"PUT": 0.3, "GET": 0.3},
						DurationSeconds: 1,
						TargetRPS:       1,
					},
					Targets: []Target{{Metric: MetricPutP99}},
				}},
			},
			wantSub: "sum to 1.0",
		},
		{
			name: "duplicate scenario",
			suite: Suite{
				Name: "s",
				Scenarios: []Scenario{
					{
						Name: "dup",
						Workload: Workload{
							RequestMix:      map[string]float64{"PUT": 1.0},
							DurationSeconds: 1,
							TargetRPS:       1,
						},
						Targets: []Target{{Metric: MetricPutP99}},
					},
					{
						Name: "dup",
						Workload: Workload{
							RequestMix:      map[string]float64{"PUT": 1.0},
							DurationSeconds: 1,
							TargetRPS:       1,
						},
						Targets: []Target{{Metric: MetricPutP99}},
					},
				},
			},
			wantSub: "duplicated",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.suite.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}
