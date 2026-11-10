package scheduler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	_ "github.com/artyomsv/marauder/backend/internal/plugins/trackers/nnmclub"
)

type nnmclubClearanceProvider struct {
	err         error
	invalidated int
}

func (p *nnmclubClearanceProvider) Clearance(context.Context, string) (registry.Clearance, error) {
	if p.err != nil {
		return registry.Clearance{}, p.err
	}
	return registry.Clearance{Cookies: map[string]string{"cf_clearance": "token"}, UserAgent: "Browser UA"}, nil
}

func (p *nnmclubClearanceProvider) InvalidateClearance(string) { p.invalidated++ }

type nnmclubRoundTrip func(*http.Request) (*http.Response, error)

func (f nnmclubRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Feed the REAL plugin's Check error into both scheduler classifiers. A raw
// 403 used to become auth even though this tracker has no credentials.
func TestNNMClubChallengeClassification(t *testing.T) {
	tracker := registry.GetTracker("nnmclub")
	if tracker == nil {
		t.Fatal("NNM-Club is not registered")
	}
	// No tests in this package run in parallel. Intercept every HTTP request
	// in memory, so even the plugin's real hostname cannot reach the network.
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, tc := range []struct {
		name      string
		provider  *nnmclubClearanceProvider
		wantCause error
		wantCode  string
		wantCalls int
	}{
		{"missing solver", nil, registry.ErrClearanceNotConfigured, errCodeSolverMissing, 1},
		{"failed solver", &nnmclubClearanceProvider{err: context.DeadlineExceeded}, registry.ErrClearanceUnavailable, errCodeSolver, 1},
		{"clearance rejected", &nnmclubClearanceProvider{}, registry.ErrCloudflareChallenge, errCodeCloudflare, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.provider == nil {
				registry.SetClearanceProvider(nil)
			} else {
				registry.SetClearanceProvider(tc.provider)
			}
			t.Cleanup(func() { registry.SetClearanceProvider(nil) })
			calls := 0
			http.DefaultTransport = nnmclubRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != "https://nnmclub.to/forum/viewtopic.php?t=420880" {
					t.Errorf("unexpected request: %s", r.URL)
				}
				h := make(http.Header)
				h.Set("Cf-Mitigated", "challenge")
				return &http.Response{
					StatusCode: http.StatusForbidden, Header: h,
					Body: io.NopCloser(strings.NewReader("opaque response")), Request: r,
				}, nil
			})
			_, err := tracker.Check(context.Background(), &domain.Topic{URL: "https://nnmclub.to/forum/viewtopic.php?t=420880"}, nil)
			if !errors.Is(err, tc.wantCause) {
				t.Fatalf("Check error = %v, want %v", err, tc.wantCause)
			}
			if got := classifyCause(err.Error(), err); got != tc.wantCode {
				t.Errorf("classifyCause = %q, want %q (never auth)", got, tc.wantCode)
			}
			if got := classifyError(err.Error()); got != tc.wantCode {
				t.Errorf("classifyError = %q, want %q (never auth)", got, tc.wantCode)
			}
			if calls != tc.wantCalls {
				t.Errorf("requests = %d, want %d", calls, tc.wantCalls)
			}
			if tc.provider != nil && tc.provider.invalidated != tc.wantCalls-1 {
				t.Errorf("invalidations = %d, want %d", tc.provider.invalidated, tc.wantCalls-1)
			}
		})
	}
}
