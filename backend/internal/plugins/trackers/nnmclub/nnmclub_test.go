package nnmclub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	"github.com/artyomsv/marauder/backend/internal/plugins/trackers/forumcommon"
	"github.com/artyomsv/marauder/backend/internal/topics"
)

const fixtureViewtopicHTML = `<html><head>
<link rel="canonical" href="https://nnmclub.to/forum/viewtopic.php?t=420880">
<title>Через тернии к звёздам (1980) DVDRip [H.264] Оригинальная версия :: NNM-Club</title>
<meta property="og:image" content="https://a.radikal.ru/a11/2008/6f/f91ffdbf65b2.jpg"/>
</head>
<body>
<a href="logout.php">logout</a>
<a rel="nofollow" href="magnet:?xt=urn:btih:094EC3052ED759240E4DFD89F3F7CA5C5B428FF4" title="Примагнититься"><img src="https://nnmstatic.win/forum/images/magnet.png"></a>
<a href="download.php?id=379398" rel="nofollow">Скачать</a>
</body></html>`

// testTopicURL is a real NNM-Club host so it passes the fetch() SSRF
// allowlist; hostRewrite redirects the actual connection to the test server.
const testTopicURL = "https://nnmclub.to/forum/viewtopic.php?t=42"

// This post's canonical link was verified with a plain GET on 2026-11-10.
const testPostURL = "https://nnmclub.to/forum/viewtopic.php?p=3820417"

func newTestPlugin(t *testing.T) *plugin {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/forum/viewtopic.php"):
			w.WriteHeader(200)
			w.Write([]byte(fixtureViewtopicHTML))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)

	return &plugin{
		sessions:  forumcommon.New(),
		transport: &hostRewrite{host: strings.TrimPrefix(srv.URL, "http://")},
	}
}

// hostRewrite reroutes every request to the test server while leaving the
// request's declared URL (a real nnmclub.to host) intact for the SSRF guard.
type hostRewrite struct{ host string }

func (h *hostRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = h.host
	return http.DefaultTransport.RoundTrip(req)
}

func TestCanParse(t *testing.T) {
	p := &plugin{}
	cases := map[string]bool{
		"https://nnmclub.to/forum/viewtopic.php?t=12345":                                  true,
		"https://www.nnmclub.to/forum/viewtopic.php?t=12345":                              true,
		"http://nnmclub.to/forum/viewtopic.php?p=3820417":                                 true,
		"https://www.nnmclub.to/forum/viewtopic.php?p=3820417#3820417":                    true,
		"https://nnmclub.to/forum/viewtopic.php?sid=abc&p=3820417&highlight=test#3820417": true,
		"https://nnmclub.to/forum/viewtopic.php?sid=abc&t=12345&start=15#3820417":         true,
		"https://nnmclub.to/forum/viewtopic.php?p=3820417&highlight=test":                 true,
		// nnmclub.me became a parked domain (2026-08-03) and was dropped from
		// knownDomains, so it must no longer parse as NNM-Club.
		"https://nnmclub.me/forum/viewtopic.php?t=12345":                     false,
		"https://nnmclub.me/forum/viewtopic.php?p=3820417":                   false,
		"https://nnmclub.to.evil.example/forum/viewtopic.php?p=1":            false,
		"https://nnmclub.to@evil.example/forum/viewtopic.php?p=1":            false,
		"https://user@nnmclub.to/forum/viewtopic.php?p=1":                    false,
		"https://nnmclub.to:443/forum/viewtopic.php?p=1":                     false,
		"https://nnmclub.to/viewtopic.php?p=1":                               false,
		"https://nnmclub.to/forum/viewtopic.php/other?p=1":                   false,
		"https://nnmclub.to/forum/%76iewtopic.php?t=42abc":                  false,
		"https://nnmclub.to./forum/viewtopic.php?t=42abc&highlight=100%":     false,
		"ftp://nnmclub.to/forum/viewtopic.php?p=1":                           false,
		"https://nnmclub.to/forum/viewtopic.php?p=12abc":                     false,
		"https://nnmclub.to/forum/viewtopic.php?t=12abc":                     true,
		"https://nnmclub.to/forum/viewtopic.php?p=0":                         false,
		"https://nnmclub.to/forum/viewtopic.php?p=-1":                        false,
		"https://nnmclub.to/forum/viewtopic.php?p=1&p=2":                     false,
		"https://nnmclub.to/forum/viewtopic.php?p=9999999999999999999999999": false,
		"https://nnmclub.to/forum/viewtopic.php?p=%ZZ":                       false,
		"https://nnmclub.to/forum/viewtopic.php?t=bad&t=42":                  false,
		"https://nnmclub.to/forum/viewtopic.php?t=&t=42":                     false,
		"https://nnmclub.to/forum/viewtopic.php?t=-1":                        false,
		"https://nnmclub.to/forum/viewtopic.php?t=9999999999999999999999999": false,
		"https://nnmclub.to/forum/viewtopic.php?notp=1":                      false,
		"https://nnmclub.to/forum/index.php":                                 false,
		"https://example.com/":                                               false,
	}
	for u, want := range cases {
		if got := p.CanParse(u); got != want {
			t.Errorf("CanParse(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestUsesCloudflare(t *testing.T) {
	p := &plugin{}
	if !p.UsesCloudflare() {
		t.Error("nnm-club should report UsesCloudflare()")
	}
}

func TestParse(t *testing.T) {
	p := &plugin{}
	for _, rawURL := range []string{
		testTopicURL,
		" http://www.nnmclub.to/forum/viewtopic.php?sid=abc&t=0042&start=15#3820417 ",
	} {
		topic, err := p.Parse(context.Background(), rawURL)
		if err != nil {
			t.Fatal(err)
		}
		if topic.Extra["topic_id"] != 42 || topic.DisplayName != "NNM-Club topic 42" || topic.URL != testTopicURL {
			t.Errorf("Parse(%q) = %+v", rawURL, topic)
		}
	}
}

func TestParse_LegacyTopicURLs(t *testing.T) {
	p := newTestPlugin(t)
	for _, tc := range []struct {
		name  string
		query string
		id    int
	}{
		{"semicolon extra", "t=42&sid=a;b", 42},
		{"HTML entity", "t=42&amp;start=15", 42},
		{"bad escape extra", "t=42&highlight=100%", 42},
		{"good pair after bad escape", "highlight=100%&t=42", 42},
		{"semicolon in first pair", "t=42;sid=abc&t=99", 42},
		{"bad escape in first pair", "t=42%ZZ&t=99", 42},
		{"empty post", "t=42&p=", 42},
		{"invalid post", "t=42&p=bad", 42},
		{"post suffix", "t=42&p=123abc", 42},
		{"zero post", "t=42&p=0", 42},
		{"negative post", "t=42&p=-1", 42},
		{"overflow post", "t=42&p=9999999999999999999999999", 42},
		{"bad post escape", "t=42&p=%ZZ", 42},
		{"duplicate post", "t=42&p=1&p=2", 42},
		{"invalid post first", "p=bad&t=42", 42},
		{"topic suffix", "t=123abc", 123},
		{"duplicate topic", "t=123&t=456", 123},
		{"duplicate topic after extra", "sid=abc&t=42&t=99", 42},
		{"invalid second topic", "t=42&t=bad", 42},
		{"escaped digits", "t=4%32", 42},
		{"zero topic", "t=0", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rawURL := "https://nnmclub.to/forum/viewtopic.php?" + tc.query
			if !p.CanParse(rawURL) {
				t.Fatalf("legacy URL no longer recognised: %s", rawURL)
			}
			topic, err := p.Parse(context.Background(), rawURL)
			if err != nil {
				t.Fatal(err)
			}
			wantURL := "https://nnmclub.to/forum/viewtopic.php?t=" + strconv.Itoa(tc.id)
			if topic.URL != wantURL || topic.Extra["topic_id"] != tc.id {
				t.Fatalf("Parse = %+v, want topic %d at %s", topic, tc.id, wantURL)
			}
			// Existing rows are not migrated: Check must accept the original
			// stored query too, not just Parse's newly normalised URL.
			check, err := p.Check(context.Background(), &domain.Topic{URL: rawURL}, nil)
			if err != nil || check == nil || check.Hash != "094ec3052ed759240e4dfd89f3f7ca5c5b428ff4" {
				t.Fatalf("Check legacy URL = %+v, %v", check, err)
			}
		})
	}
}

func TestParse_PostResolvesToTopic(t *testing.T) {
	p := newTestPlugin(t)
	for _, rawURL := range []string{
		testPostURL,
		"https://nnmclub.to/forum/viewtopic.php?sid=abc&p=3820417&highlight=test#3820417",
		"https://nnmclub.to/forum/viewtopic.php?p=3820417&highlight=100%",
		"https://nnmclub.to/forum/viewtopic.php?sid=a;b&p=3820417",
		// A post selector must not be mistaken for the accompanying topic ID.
		"https://nnmclub.to/forum/viewtopic.php?t=999&p=3820417",
	} {
		topic, err := p.Parse(context.Background(), rawURL)
		if err != nil {
			t.Fatal(err)
		}
		if topic.URL != "https://nnmclub.to/forum/viewtopic.php?t=420880" ||
			topic.Extra["topic_id"] != 420880 || topic.DisplayName != "NNM-Club topic 420880" {
			t.Errorf("resolved topic = %+v", topic)
		}
		if _, ok := topic.Extra["post_id"]; ok {
			t.Errorf("resolved topic retains a post identity: %v", topic.Extra)
		}
	}
}

func TestParse_PostResolutionFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"cloudflare", http.StatusForbidden, "challenge"},
		{"unavailable", http.StatusServiceUnavailable, "maintenance"},
		{"missing post", http.StatusNotFound, "not found"},
		{"missing canonical", http.StatusOK, "<html><head></head><body></body></html>"},
		{"off-site canonical", http.StatusOK, `<html><head><link rel="canonical" href="https://evil.example/forum/viewtopic.php?t=42"></head></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RawQuery != "p=3820417" {
					t.Errorf("lookup query = %q", r.URL.RawQuery)
				}
				if tc.name == "cloudflare" {
					w.Header().Set("Cf-Mitigated", "challenge")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			p := &plugin{sessions: forumcommon.New(), transport: &hostRewrite{host: strings.TrimPrefix(srv.URL, "http://")}}
			topic, err := p.Parse(context.Background(), testPostURL+"&highlight=test#3820417")
			if err != nil {
				t.Fatalf("resolution must fail open: %v", err)
			}
			if topic.URL != testPostURL || topic.Extra["post_id"] != 3820417 || topic.DisplayName != "NNM-Club post 3820417" {
				t.Errorf("fallback topic = %+v", topic)
			}
			if _, ok := topic.Extra["topic_id"]; ok {
				t.Errorf("post ID stored as a topic ID: %v", topic.Extra)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParse_PostResolutionBudget(t *testing.T) {
	for _, budget := range []time.Duration{time.Minute, time.Second} {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > min(budget, postResolveTimeout) {
				t.Errorf("resolve deadline = %v, want a live bounded context", deadline)
			}
			return nil, context.DeadlineExceeded
		})}
		topic, err := p.Parse(ctx, testPostURL)
		if err != nil || topic == nil || topic.URL != testPostURL {
			t.Errorf("timeout must retain post URL: topic=%+v, err=%v", topic, err)
		}
		if ctx.Err() != nil {
			t.Errorf("resolve cancelled the creator's context: %v", ctx.Err())
		}
		cancel()
	}
}

func TestParse_RejectsOffSiteBeforeResolve(t *testing.T) {
	p := &plugin{} // no session: neither URL may reach the network
	for _, param := range []string{"t=42", "p=3820417"} {
		if _, err := p.Parse(context.Background(), "https://evil.example/forum/viewtopic.php?"+param); err == nil {
			t.Errorf("Parse accepted off-site %s", param)
		}
	}
}

func TestParse_PostUsesActiveMirrorButStoresCanonicalDomain(t *testing.T) {
	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == pluginName {
			return registry.DomainConfig{Active: "nnmclub.mirror", Custom: []string{"nnmclub.mirror"}}
		}
		return registry.DomainConfig{}
	})
	t.Cleanup(func() { registry.SetDomainResolver(nil) })
	p := newTestPlugin(t)
	rec := &hostRecordingRewrite{target: p.transport.(*hostRewrite).host}
	p.transport = rec
	topic, err := p.Parse(context.Background(), "https://nnmclub.mirror/forum/viewtopic.php?sid=abc&p=3820417#3820417")
	if err != nil {
		t.Fatal(err)
	}
	if topic.URL != "https://nnmclub.to/forum/viewtopic.php?t=420880" || !reflect.DeepEqual(rec.hosts, []string{"nnmclub.mirror"}) {
		t.Errorf("topic URL = %q, fetch hosts = %v", topic.URL, rec.hosts)
	}
}

func TestCheck(t *testing.T) {
	p := newTestPlugin(t)
	topic := &domain.Topic{URL: testTopicURL}
	check, err := p.Check(context.Background(), topic, nil)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if check.Hash != "094ec3052ed759240e4dfd89f3f7ca5c5b428ff4" {
		t.Errorf("hash: %q", check.Hash)
	}
	if !strings.Contains(check.DisplayName, "Через тернии к звёздам") {
		t.Errorf("display name: %q", check.DisplayName)
	}
	if strings.HasSuffix(check.DisplayName, " :: NNM-Club") {
		t.Errorf("site suffix not stripped: %q", check.DisplayName)
	}
}

func TestDownload(t *testing.T) {
	p := newTestPlugin(t)
	topic := &domain.Topic{URL: testTopicURL}
	payload, err := p.Download(context.Background(), topic, nil, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(payload.TorrentFile) != 0 {
		t.Errorf("expected no torrent file in anonymous mode, got %d bytes", len(payload.TorrentFile))
	}
	if !strings.Contains(payload.MagnetURI, "urn:btih:094ec3052ed759240e4dfd89f3f7ca5c5b428ff4") {
		t.Errorf("magnet missing infohash: %q", payload.MagnetURI)
	}
	if !strings.Contains(payload.MagnetURI, "dn=") {
		t.Errorf("magnet missing display name: %q", payload.MagnetURI)
	}
}

func TestFetch_RejectsOffSiteHost(t *testing.T) {
	// The SSRF guard must refuse any topic URL that is not an NNM-Club host,
	// before a request is ever dialed — so no test server is needed here.
	p := &plugin{sessions: forumcommon.New()}
	bad := []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://localhost:6379/",
		"https://evil.com/forum/viewtopic.php?t=1",
		"https://nnmclub.to.evil.com/forum/",
		"ftp://nnmclub.to/forum/",
		"://malformed",
	}
	for _, target := range bad {
		if _, err := p.Check(context.Background(), &domain.Topic{URL: target}, nil); err == nil {
			t.Errorf("Check(%q) should be refused by the SSRF guard", target)
		}
		if _, err := p.ResolveMetadata(context.Background(), target, nil); err == nil {
			t.Errorf("ResolveMetadata(%q) should be refused by the SSRF guard", target)
		}
	}
}

func TestResolveMetadata(t *testing.T) {
	p := newTestPlugin(t)
	meta, err := p.ResolveMetadata(context.Background(), testTopicURL, nil)
	if err != nil {
		t.Fatalf("ResolveMetadata: %v", err)
	}
	if !strings.Contains(meta.Title, "Через тернии к звёздам") {
		t.Errorf("title: %q", meta.Title)
	}
	if meta.ImageURL != "https://a.radikal.ru/a11/2008/6f/f91ffdbf65b2.jpg" {
		t.Errorf("image url: %q", meta.ImageURL)
	}
}

func TestPostURL_Readers(t *testing.T) {
	readers := []struct {
		name string
		read func(*testing.T, *plugin)
	}{
		{"Check", func(t *testing.T, p *plugin) {
			check, err := p.Check(context.Background(), &domain.Topic{URL: testPostURL}, nil)
			if err != nil || check == nil || check.Hash != "094ec3052ed759240e4dfd89f3f7ca5c5b428ff4" {
				t.Fatalf("Check = %+v, %v", check, err)
			}
		}},
		{"Download", func(t *testing.T, p *plugin) {
			payload, err := p.Download(context.Background(), &domain.Topic{URL: testPostURL}, nil, nil)
			if err != nil || payload == nil || !strings.Contains(payload.MagnetURI, "urn:btih:094ec3052ed759240e4dfd89f3f7ca5c5b428ff4&dn=") {
				t.Fatalf("Download = %+v, %v", payload, err)
			}
		}},
		{"ResolveMetadata", func(t *testing.T, p *plugin) {
			meta, err := p.ResolveMetadata(context.Background(), testPostURL, nil)
			if err != nil || meta == nil || !strings.Contains(meta.Title, "Через тернии к звёздам") ||
				meta.ImageURL != "https://a.radikal.ru/a11/2008/6f/f91ffdbf65b2.jpg" {
				t.Fatalf("ResolveMetadata = %+v, %v", meta, err)
			}
		}},
	}
	for _, tc := range []struct {
		name string
		page string
		want []string
	}{
		{
			// Synthetic later-post page with no release content: finding the
			// canonical link must cause a separate fetch of page one.
			"later page",
			`<html><head><link href="viewtopic.php?sid=abc&amp;t=420880&amp;start=15#3820417" rel="canonical"></head><body>Replies only</body></html>`,
			[]string{"p=3820417", "t=420880"},
		},
		{
			"unresolved but has release",
			strings.Replace(fixtureViewtopicHTML, `<link rel="canonical" href="https://nnmclub.to/forum/viewtopic.php?t=420880">`, "", 1),
			[]string{"p=3820417"},
		},
	} {
		for _, reader := range readers {
			t.Run(tc.name+"/"+reader.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.RawQuery {
					case "p=3820417":
						_, _ = w.Write([]byte(tc.page))
					case "t=420880":
						_, _ = w.Write([]byte(fixtureViewtopicHTML))
					default:
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(srv.Close)
				rec := &hostRecordingRewrite{target: strings.TrimPrefix(srv.URL, "http://")}
				p := &plugin{sessions: forumcommon.New(), transport: rec}
				reader.read(t, p)
				if !reflect.DeepEqual(rec.queries, tc.want) {
					t.Errorf("queries = %v, want %v", rec.queries, tc.want)
				}
			})
		}
	}
}

func TestTopicIDFromPage(t *testing.T) {
	for _, tc := range []struct {
		name string
		page string
		want int
	}{
		{"live canonical", fixtureViewtopicHTML, 420880},
		{"relative and reordered", `<head><link href='viewtopic.php?sid=x&amp;t=42&amp;start=15#10' rel='canonical'></head>`, 42},
		{"root relative", `<head><link rel=canonical href='/forum/viewtopic.php?t=42'></head>`, 42},
		{"off-site", `<head><link rel="canonical" href="https://evil.example/forum/viewtopic.php?t=42"></head>`, 0},
		{"wrong path", `<head><link rel="canonical" href="/viewtopic.php?t=42"></head>`, 0},
		{"post is not topic", `<head><link rel="canonical" href="viewtopic.php?p=42"></head>`, 0},
		{"legacy topic suffix", `<head><link rel="canonical" href="viewtopic.php?t=42bad"></head>`, 42},
		{"malformed ID", `<head><link rel="canonical" href="viewtopic.php?t=bad42"></head>`, 0},
		{"body link ignored", `<head></head><body><link rel="canonical" href="viewtopic.php?t=99"></body>`, 0},
		{"navigation ignored", `<head></head><body><a href="viewtopic.php?t=99">Other topic</a></body>`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := topicIDFromPage([]byte(tc.page), testPostURL); got != tc.want {
				t.Errorf("topicIDFromPage = %d, want %d", got, tc.want)
			}
		})
	}
}

// urlStore models the repository's (user_id, url) uniqueness constraint so
// the real NNM-Club Parse and shared API/Sonarr create path are exercised.
type urlStore struct {
	rows map[string]*domain.Topic
}

func (s *urlStore) Create(_ context.Context, topic *domain.Topic) (*domain.Topic, error) {
	key := topic.UserID.String() + " " + topic.URL
	if _, exists := s.rows[key]; exists {
		return nil, &pgconn.PgError{Code: "23505"}
	}
	s.rows[key] = topic
	return topic, nil
}

func TestBuildAndCreate_PostIdentity(t *testing.T) {
	// No tests in this package run in parallel. Keep the registered plugin
	// but temporarily give it the same isolated transport as the unit tests.
	registered := registry.GetTracker(pluginName).(*plugin)
	original := *registered
	t.Cleanup(func() { *registered = original })
	*registered = *newTestPlugin(t)
	for _, tc := range []struct {
		name   string
		source string
		first  string
		second string
	}{
		{"API topic first", "", "https://nnmclub.to/forum/viewtopic.php?t=420880", testPostURL + "&highlight=test#3820417"},
		{"Sonarr topic first", "sonarr", "https://nnmclub.to/forum/viewtopic.php?t=420880", testPostURL},
		{"post first", "", testPostURL, "https://nnmclub.to/forum/viewtopic.php?sid=abc&t=420880&start=15#3820417"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &urlStore{rows: map[string]*domain.Topic{}}
			input := topics.CreateInput{UserID: uuid.New(), Source: tc.source, URL: tc.first}
			first, err := topics.BuildAndCreate(context.Background(), store, input)
			if err != nil || first == nil || !first.Created {
				t.Fatalf("initial create = %+v, %v", first, err)
			}
			input.URL = tc.second
			if registry.FindTrackerForURL(input.URL) != registered {
				t.Fatal("post URL not matched to NNM-Club")
			}
			duplicate, err := topics.BuildAndCreate(context.Background(), store, input)
			if err != nil || duplicate == nil || duplicate.Created || duplicate.Topic != nil || len(store.rows) != 1 {
				t.Fatalf("post alias must be an idempotent duplicate: %+v, %v, rows=%v", duplicate, err, store.rows)
			}
		})
	}
}

func TestBuildAndCreate_PostResolveFailureStillCreates(t *testing.T) {
	registered := registry.GetTracker(pluginName).(*plugin)
	original := *registered
	t.Cleanup(func() { *registered = original })
	*registered = plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("tracker unavailable")
	})}
	store := &urlStore{rows: map[string]*domain.Topic{}}
	res, err := topics.BuildAndCreate(context.Background(), store, topics.CreateInput{UserID: uuid.New(), URL: testPostURL, Source: "sonarr"})
	if err != nil || res == nil || !res.Created || res.Topic.URL != testPostURL || !res.Topic.DisplayNameIsPlaceholder {
		t.Fatalf("failed resolve must still create post topic: %+v, %v", res, err)
	}
}

func TestCanParse_CustomDomain_AllowedViaResolver(t *testing.T) {
	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == "nnmclub" {
			return registry.DomainConfig{Custom: []string{"nnmclub.example"}}
		}
		return registry.DomainConfig{}
	})
	t.Cleanup(func() { registry.SetDomainResolver(nil) })
	p := &plugin{}
	for _, param := range []string{"t=123", "sid=abc&p=456#456"} {
		if !p.CanParse("https://nnmclub.example/forum/viewtopic.php?" + param) {
			t.Errorf("custom domain should parse: %s", param)
		}
		if p.CanParse("https://evil.example/forum/viewtopic.php?" + param) {
			t.Errorf("unlisted domain must not parse: %s", param)
		}
	}
}

func TestDomains_CanonicalFirst(t *testing.T) {
	p := &plugin{}
	want := []string{"nnmclub.to"}
	if !reflect.DeepEqual(p.Domains(), want) {
		t.Errorf("Domains() = %v, want %v", p.Domains(), want)
	}
}

// hostRecordingRewrite records the Host of every outgoing request, then
// redirects it to the test server (target) over http so no real network is
// hit. Mirrors the kinozal test helper of the same purpose.
type hostRecordingRewrite struct {
	target  string
	hosts   []string
	queries []string
}

func (h *hostRecordingRewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	h.hosts = append(h.hosts, req.URL.Host)
	h.queries = append(h.queries, req.URL.RawQuery)
	req.URL.Scheme = "http"
	req.URL.Host = h.target
	return http.DefaultTransport.RoundTrip(req)
}

// TestCheck_RewritesToActiveDomain asserts that when the admin has
// configured an active domain override, Check fetches that host instead of
// the mirror recorded in the stored topic URL — NNM-Club has no domain
// field, so fetchTopicPage must apply the override before fetching.
func TestCheck_RewritesToActiveDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(fixtureViewtopicHTML))
	}))
	t.Cleanup(srv.Close)

	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == "nnmclub" {
			return registry.DomainConfig{Active: "nnmclub.mirror", Custom: []string{"nnmclub.mirror"}}
		}
		return registry.DomainConfig{}
	})
	t.Cleanup(func() { registry.SetDomainResolver(nil) })

	rec := &hostRecordingRewrite{target: strings.TrimPrefix(srv.URL, "http://")}
	p := &plugin{sessions: forumcommon.New(), transport: rec}

	topic := &domain.Topic{URL: "https://nnmclub.to/forum/viewtopic.php?t=42"}
	if _, err := p.Check(context.Background(), topic, nil); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(rec.hosts) == 0 {
		t.Fatal("no requests recorded")
	}
	if rec.hosts[0] != "nnmclub.mirror" {
		t.Errorf("fetch host = %q, want active domain nnmclub.mirror", rec.hosts[0])
	}
}

// TestDownload_RewritesToActiveDomain mirrors TestCheck_RewritesToActiveDomain
// for Download: during a primary-domain outage with an admin-configured
// active mirror, Download must fetch the mirror host too — otherwise Check
// detects releases via the mirror while Download keeps dialing the dead
// host, an endless release.found/download-fail loop.
func TestDownload_RewritesToActiveDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(fixtureViewtopicHTML))
	}))
	t.Cleanup(srv.Close)

	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == "nnmclub" {
			return registry.DomainConfig{Active: "nnmclub.mirror", Custom: []string{"nnmclub.mirror"}}
		}
		return registry.DomainConfig{}
	})
	t.Cleanup(func() { registry.SetDomainResolver(nil) })

	rec := &hostRecordingRewrite{target: strings.TrimPrefix(srv.URL, "http://")}
	p := &plugin{sessions: forumcommon.New(), transport: rec}

	topic := &domain.Topic{URL: "https://nnmclub.to/forum/viewtopic.php?t=42"}
	if _, err := p.Download(context.Background(), topic, nil, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if len(rec.hosts) == 0 {
		t.Fatal("no requests recorded")
	}
	if rec.hosts[0] != "nnmclub.mirror" {
		t.Errorf("fetch host = %q, want active domain nnmclub.mirror", rec.hosts[0])
	}
}

// TestResolveMetadata_RewritesToActiveDomain mirrors
// TestCheck_RewritesToActiveDomain for ResolveMetadata.
func TestResolveMetadata_RewritesToActiveDomain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(fixtureViewtopicHTML))
	}))
	t.Cleanup(srv.Close)

	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == "nnmclub" {
			return registry.DomainConfig{Active: "nnmclub.mirror", Custom: []string{"nnmclub.mirror"}}
		}
		return registry.DomainConfig{}
	})
	t.Cleanup(func() { registry.SetDomainResolver(nil) })

	rec := &hostRecordingRewrite{target: strings.TrimPrefix(srv.URL, "http://")}
	p := &plugin{sessions: forumcommon.New(), transport: rec}

	if _, err := p.ResolveMetadata(context.Background(), "https://nnmclub.to/forum/viewtopic.php?t=42", nil); err != nil {
		t.Fatalf("ResolveMetadata: %v", err)
	}
	if len(rec.hosts) == 0 {
		t.Fatal("no requests recorded")
	}
	if rec.hosts[0] != "nnmclub.mirror" {
		t.Errorf("fetch host = %q, want active domain nnmclub.mirror", rec.hosts[0])
	}
}
