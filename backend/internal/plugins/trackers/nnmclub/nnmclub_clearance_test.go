package nnmclub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/flaresolverr"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	"github.com/artyomsv/marauder/backend/internal/plugins/trackers/forumcommon"
)

// Like the Kinozal/RuTracker clearance tests, install a provider through the
// registry seam. Nothing in this file dials NNM-Club or a real solver.
type fakeClearanceProvider struct {
	c           registry.Clearance
	err         error
	refreshed   registry.Clearance
	probes      []string
	invalidated []string
}

func (p *fakeClearanceProvider) Clearance(_ context.Context, probeURL string) (registry.Clearance, error) {
	p.probes = append(p.probes, probeURL)
	return p.c, p.err
}

func (p *fakeClearanceProvider) InvalidateClearance(probeURL string) {
	p.invalidated = append(p.invalidated, probeURL)
	if p.refreshed.Valid() {
		p.c = p.refreshed
	}
}

func withClearanceProvider(t *testing.T, p registry.ClearanceProvider) {
	t.Helper()
	// These tests are deliberately serial: the registry hook is process-wide.
	registry.SetClearanceProvider(p)
	t.Cleanup(func() { registry.SetClearanceProvider(nil) })
}

func testClearance() registry.Clearance {
	return registry.Clearance{
		Cookies:   map[string]string{"cf_clearance": "minted-token"},
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) Chrome/130.0.0.0",
	}
}

func clearanceResponse(r *http.Request, status int, challenged bool, body string) *http.Response {
	h := make(http.Header)
	if challenged {
		h.Set("Cf-Mitigated", "challenge")
	}
	return &http.Response{
		StatusCode: status, Header: h,
		Body: io.NopCloser(strings.NewReader(body)), Request: r,
	}
}

func hasClearance(r *http.Request, c registry.Clearance) bool {
	cookie, err := r.Cookie("cf_clearance")
	return err == nil && cookie.Value == c.Cookies["cf_clearance"] && r.UserAgent() == c.UserAgent
}

func TestFetch_ChallengeCauses(t *testing.T) {
	boom := errors.New("flaresolverr: connection refused")
	for _, tc := range []struct {
		name     string
		provider *fakeClearanceProvider
		want     error
	}{
		{"no provider", nil, registry.ErrClearanceNotConfigured},
		{"failed provider", &fakeClearanceProvider{err: boom}, registry.ErrClearanceUnavailable},
	} {
		for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
			t.Run(tc.name+"/"+strconv.Itoa(status), func(t *testing.T) {
				if tc.provider == nil {
					withClearanceProvider(t, nil)
				} else {
					// Fresh recording state for each status.
					fp := &fakeClearanceProvider{err: tc.provider.err}
					withClearanceProvider(t, fp)
					defer func() {
						if len(fp.probes) != 1 || len(fp.invalidated) != 0 {
							t.Errorf("provider calls = %v, invalidations = %v; failed mints must not retry", fp.probes, fp.invalidated)
						}
					}()
				}
				calls := 0
				p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					// No challenge wording: the header and status are the signal.
					return clearanceResponse(r, status, true, "opaque response"), nil
				})}
				_, err := p.Check(context.Background(), &domain.Topic{URL: testTopicURL}, nil)
				if !errors.Is(err, tc.want) || errors.Is(err, registry.ErrCloudflareChallenge) {
					t.Fatalf("Check error = %v, want only %v", err, tc.want)
				}
				if tc.provider != nil && !errors.Is(err, boom) {
					t.Errorf("provider's cause was lost: %v", err)
				}
				if calls != 1 {
					t.Errorf("requests = %d, want 1", calls)
				}
			})
		}
	}
}

func TestFetch_StaleClearanceRetriesExactlyOnce(t *testing.T) {
	for _, stillBlocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("still blocked=%v", stillBlocked), func(t *testing.T) {
			stale := registry.Clearance{Cookies: map[string]string{"cf_clearance": "stale-token"}, UserAgent: "Old browser UA"}
			fp := &fakeClearanceProvider{c: stale, refreshed: testClearance()}
			withClearanceProvider(t, fp)
			calls := 0
			p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				want := testClearance()
				if calls == 1 {
					want = stale
				}
				if !hasClearance(r, want) {
					t.Errorf("request %d: cookie/UA = %q / %q, want the paired clearance", calls, r.Header.Get("Cookie"), r.UserAgent())
				}
				if calls == 1 || stillBlocked {
					return clearanceResponse(r, http.StatusForbidden, true, "opaque response"), nil
				}
				return clearanceResponse(r, http.StatusOK, false, fixtureViewtopicHTML), nil
			})}
			body, err := p.fetch(context.Background(), testTopicURL, nil)
			if stillBlocked {
				if !errors.Is(err, registry.ErrCloudflareChallenge) || errors.Is(err, registry.ErrClearanceUnavailable) {
					t.Errorf("error = %v, want ErrCloudflareChallenge after the second block", err)
				}
			} else if err != nil || string(body) != fixtureViewtopicHTML {
				t.Errorf("retry returned body=%q, error=%v", body, err)
			}
			if calls != 2 || !reflect.DeepEqual(fp.probes, []string{testTopicURL, testTopicURL}) ||
				!reflect.DeepEqual(fp.invalidated, []string{testTopicURL}) {
				t.Errorf("requests=%d, probes=%v, invalidations=%v; want two attempts and one invalidation of the requested page", calls, fp.probes, fp.invalidated)
			}
		})
	}
}

func TestFetch_UngatedWithoutClearance(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider failed=%v", failed), func(t *testing.T) {
			if failed {
				withClearanceProvider(t, &fakeClearanceProvider{err: errors.New("solver down")})
			} else {
				withClearanceProvider(t, nil)
			}
			calls := 0
			p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.UserAgent() != userAgent || r.Header.Get("Cookie") != "" {
					t.Errorf("unadorned request = UA %q, cookies %q", r.UserAgent(), r.Header.Get("Cookie"))
				}
				return clearanceResponse(r, http.StatusOK, false, fixtureViewtopicHTML), nil
			})}
			body, err := p.fetch(context.Background(), testTopicURL, nil)
			if err != nil || string(body) != fixtureViewtopicHTML || calls != 1 {
				t.Fatalf("ungated fetch: body=%q, error=%v, requests=%d", body, err, calls)
			}
		})
	}
}

func TestFetch_NonChallengeStatusesKeepTheirError(t *testing.T) {
	for _, status := range []int{403, 404, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			fp := &fakeClearanceProvider{c: testClearance()}
			withClearanceProvider(t, fp)
			calls := 0
			p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				// Body text must not be mistaken for a Cf-Mitigated header.
				return clearanceResponse(r, status, false, "Cloudflare challenge: Just a moment..."), nil
			})}
			_, err := p.fetch(context.Background(), testTopicURL, nil)
			want := fmt.Sprintf("nnm-club GET %s -> %d", testTopicURL, status)
			if err == nil || err.Error() != want || calls != 1 || len(fp.invalidated) != 0 {
				t.Errorf("error=%v, requests=%d, invalidations=%v; want %q without retry", err, calls, fp.invalidated, want)
			}
		})
	}
}

func TestClearance_AllPageReaders(t *testing.T) {
	canonical := "https://nnmclub.to/forum/viewtopic.php?t=420880"
	for _, reader := range []string{"Parse", "Check", "Download", "ResolveMetadata", "AuthorComment"} {
		t.Run(reader, func(t *testing.T) {
			fp := &fakeClearanceProvider{c: testClearance()}
			withClearanceProvider(t, fp)
			var fetched []string
			p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				fetched = append(fetched, r.URL.String())
				if !hasClearance(r, testClearance()) {
					t.Errorf("%s: missing clearance cookie/browser UA: %v", r.URL, r.Header)
					return clearanceResponse(r, http.StatusForbidden, true, "opaque response"), nil
				}
				var body string
				switch r.URL.RawQuery {
				case "p=3820417":
					body = `<html><head><link rel="canonical" href="` + canonical + `"></head><body>Later replies only</body></html>`
				case "t=420880":
					body = strings.Replace(fixtureViewtopicHTML, "</body>",
						nnmPost("100", "uploader", "Release")+`<a href="viewtopic.php?t=420880&amp;start=15">2</a></body>`, 1)
				case "t=420880&start=15":
					body = nnmTopicPage(nnmPost("200", "uploader", "Added episode 8."))
				case "id=379398":
					// The .torrent, which this site does not serve: Download
					// falls back to the page's magnet.
					return clearanceResponse(r, http.StatusNotFound, false, ""), nil
				default:
					t.Errorf("unexpected URL: %s", r.URL)
					return clearanceResponse(r, http.StatusNotFound, false, ""), nil
				}
				return clearanceResponse(r, http.StatusOK, false, body), nil
			})}
			ctx := context.Background()
			topic := &domain.Topic{URL: testPostURL}
			want := []string{testPostURL, canonical}
			var wantProbes []string // nil: the same as the requests
			switch reader {
			case "Parse":
				got, err := p.Parse(ctx, testPostURL)
				if err != nil || got == nil || got.URL != canonical {
					t.Fatalf("Parse = %+v, %v", got, err)
				}
				want = []string{testPostURL}
			case "Check":
				got, err := p.Check(ctx, topic, nil)
				if err != nil || got == nil || got.Hash != "094ec3052ed759240e4dfd89f3f7ca5c5b428ff4" {
					t.Fatalf("Check = %+v, %v", got, err)
				}
			case "Download":
				got, err := p.Download(ctx, topic, nil, nil)
				if err != nil || got == nil || !strings.Contains(got.MagnetURI, "urn:btih:094ec3052ed759240e4dfd89f3f7ca5c5b428ff4&dn=") {
					t.Fatalf("Download = %+v, %v", got, err)
				}
				// The .torrent request carries the clearance too, but its probe
				// is the topic page: a download URL is not a page a solver opens.
				wantProbes = []string{testPostURL, canonical, canonical}
				want = append(want, "https://nnmclub.to/forum/download.php?id=379398")
			case "ResolveMetadata":
				got, err := p.ResolveMetadata(ctx, testPostURL, nil)
				if err != nil || got == nil || got.Title == "" || got.ImageURL == "" {
					t.Fatalf("ResolveMetadata = %+v, %v", got, err)
				}
			case "AuthorComment":
				got, err := p.AuthorComment(ctx, testPostURL, nil)
				if err != nil || got != "Added episode 8." {
					t.Fatalf("AuthorComment = %q, %v", got, err)
				}
				want = append(want, canonical+"&start=15")
			}
			if wantProbes == nil {
				wantProbes = want
			}
			if !reflect.DeepEqual(fetched, want) || !reflect.DeepEqual(fp.probes, wantProbes) {
				t.Errorf("requests=%v, probes=%v, want requests %v and probes %v (probe each requested page, never the root)",
					fetched, fp.probes, want, wantProbes)
			}
		})
	}
}

func TestFetch_RejectsOffSiteBeforeClearance(t *testing.T) {
	fp := &fakeClearanceProvider{c: testClearance()}
	withClearanceProvider(t, fp)
	p := &plugin{} // no sessions: every URL must fail before a jar or dial
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"https://nnmclub.to.evil.example/forum/viewtopic.php?t=1",
		"ftp://nnmclub.to/forum/viewtopic.php?t=1",
	} {
		if _, err := p.fetch(context.Background(), target, nil); err == nil {
			t.Errorf("fetch accepted %s", target)
		}
	}
	if len(fp.probes) != 0 || len(fp.invalidated) != 0 {
		t.Errorf("untrusted target reached the provider: probes=%v, invalidations=%v", fp.probes, fp.invalidated)
	}
}

func TestChallengeProbeURL_JoinsWarmupOnActiveTopicHost(t *testing.T) {
	probe, ok := registry.GetTracker(pluginName).(registry.WithChallengeProbe)
	if !ok {
		t.Fatal("registered NNM-Club is absent from the boot warm-up capability")
	}
	registry.SetDomainResolver(nil)
	t.Cleanup(func() { registry.SetDomainResolver(nil) })
	if got := probe.ChallengeProbeURL(); got != "https://nnmclub.to/forum/viewtopic.php?t=420880" {
		t.Fatalf("boot probe = %q, want the field-tested topic page", got)
	}
	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == pluginName {
			return registry.DomainConfig{Active: "nnmclub.mirror", Custom: []string{"nnmclub.mirror"}}
		}
		return registry.DomainConfig{}
	})
	if got := probe.ChallengeProbeURL(); got != "https://nnmclub.mirror/forum/viewtopic.php?t=420880" {
		t.Errorf("boot probe = %q, want the active host's topic path", got)
	}
}

// Use the REAL per-host cache and solveBudget here, with only the solver's
// HTTP transport faked. A stub returning ErrBudgetTooShort would not prove
// that Parse avoided starting a browser or that warm-up primes its p= lookup.
func TestParse_PostClearanceBudget(t *testing.T) {
	for _, state := range []string{"cold", "warm", "stale"} {
		t.Run(state, func(t *testing.T) {
			var commands, solved []string
			solver := flaresolverr.New("http://solver.test", time.Minute)
			solver.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var request struct {
					Cmd string `json:"cmd"`
					URL string `json:"url"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					return nil, err
				}
				commands = append(commands, request.Cmd)
				body := `{"status":"ok"}`
				switch request.Cmd {
				case "sessions.create":
					body = `{"status":"ok","session":"test-session"}`
				case "request.get":
					solved = append(solved, request.URL)
					body = `{"status":"ok","solution":{"status":200,"userAgent":"` + testClearance().UserAgent +
						`","cookies":[{"name":"cf_clearance","value":"minted-token"}]}}`
				case "sessions.destroy":
				default:
					t.Errorf("unexpected solver command: %s", request.Cmd)
				}
				return clearanceResponse(r, http.StatusOK, false, body), nil
			})}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = solver.Close(ctx)
			})
			withClearanceProvider(t, solver)
			calls := 0
			parsing := true
			p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if parsing {
					deadline, ok := r.Context().Deadline()
					if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > postResolveTimeout {
						t.Errorf("Parse request has no live 5s budget: %v", deadline)
					}
				}
				if (parsing && state == "stale") || !hasClearance(r, testClearance()) {
					return clearanceResponse(r, http.StatusForbidden, true, "opaque response"), nil
				}
				return clearanceResponse(r, http.StatusOK, false, fixtureViewtopicHTML), nil
			})}
			if state != "cold" {
				// Same enumeration capability and call as cmd/server's warm-up.
				probe := registry.Tracker(p).(registry.WithChallengeProbe)
				if _, err := registry.ClearanceFor(context.Background(), probe.ChallengeProbeURL()); err != nil {
					t.Fatalf("warm-up: %v", err)
				}
				if !reflect.DeepEqual(solved, []string{p.ChallengeProbeURL()}) {
					t.Fatalf("warm-up solved %v, want the known challenged topic", solved)
				}
			}
			before := len(commands)
			topic, err := p.Parse(context.Background(), testPostURL)
			if err != nil || topic == nil {
				t.Fatalf("Parse must fail open: topic=%+v, error=%v", topic, err)
			}
			if len(commands) != before {
				t.Errorf("Parse called the solver: commands=%v; a 5s caller must only reuse a cached clearance", commands[before:])
			}
			wantCalls := 1
			if state == "warm" {
				if topic.URL != "https://nnmclub.to/forum/viewtopic.php?t=420880" || topic.Extra["topic_id"] != 420880 {
					t.Errorf("warm Parse failed to resolve post: %+v", topic)
				}
			} else {
				if topic.URL != testPostURL || topic.Extra["post_id"] != 3820417 || topic.DisplayName != "NNM-Club post 3820417" {
					t.Errorf("cold/stale Parse must retain the post identity: %+v", topic)
				}
				if _, ok := topic.Extra["topic_id"]; ok {
					t.Errorf("unresolved post acquired a topic ID: %v", topic.Extra)
				}
				if state == "stale" {
					wantCalls = 2 // invalidate once; refused cold mint, then direct fetch
				}
			}
			if calls != wantCalls {
				t.Errorf("Parse requests=%d, want %d", calls, wantCalls)
			}
			if state == "cold" {
				// The later scheduler check has room to mint, resolves the
				// retained p= URL and then reuses the cache to fetch page one.
				parsing = false
				ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
				defer cancel()
				check, err := p.Check(ctx, topic, nil)
				if err != nil || check == nil || check.Hash != "094ec3052ed759240e4dfd89f3f7ca5c5b428ff4" {
					t.Fatalf("check-time lazy mint: check=%+v, error=%v", check, err)
				}
				if !reflect.DeepEqual(solved, []string{testPostURL}) || calls != 3 {
					t.Errorf("check-time solves=%v, total tracker requests=%d; want one p= solve and two check fetches", solved, calls)
				}
			}
		})
	}
}
