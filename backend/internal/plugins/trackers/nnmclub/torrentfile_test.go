package nnmclub

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/infohash"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	"github.com/artyomsv/marauder/backend/internal/plugins/trackers/forumcommon"
	"github.com/artyomsv/marauder/backend/internal/torrentmeta/torrentmetatest"
)

// The redirect a guest gets from download.php, as measured on 2026-11-14.
const bulkLocation = "//bulk.nnmclub.to/download.php?csid=d02f911f66e90b47&uid=-1&id=379398"

// testTorrent builds a small torrent and the topic page that names it: the
// fixture page with its magnet hash replaced by the torrent's own.
func testTorrent(t *testing.T) (torrent []byte, hash, page string) {
	t.Helper()
	torrent = torrentmetatest.Torrent("Cherez.ternii.k.zvezdam[nnm-club]1980", []domain.TorrentFile{
		{Path: "film.avi", Size: 2771038000},
		{Path: "sample.avi", Size: 260},
	})
	hash, err := infohash.FromTorrent(torrent)
	if err != nil {
		t.Fatalf("test torrent: %v", err)
	}
	page = strings.Replace(fixtureViewtopicHTML, "094EC3052ED759240E4DFD89F3F7CA5C5B428FF4", strings.ToUpper(hash), 1)
	if page == fixtureViewtopicHTML {
		t.Fatal("fixture page has no magnet hash to replace")
	}
	return torrent, hash, page
}

// downloadSite serves the topic page, and hands the two download hops to the
// test: forum is /forum/download.php on the forum host, bulk is /download.php
// on the download host.
func downloadSite(t *testing.T, page string, forum, bulk http.HandlerFunc) *hostRecordingRewrite {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case topicPath:
			_, _ = w.Write([]byte(page))
		case downloadPath:
			forum(w, r)
		case "/download.php":
			bulk(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return &hostRecordingRewrite{target: strings.TrimPrefix(srv.URL, "http://")}
}

func redirectTo(location string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusFound)
	}
}

func serve(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		_, _ = w.Write(body)
	}
}

func unreachable(t *testing.T, what string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("%s was requested", what)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func TestDownload_TorrentFile(t *testing.T) {
	torrent, _, page := testTorrent(t)
	rec := downloadSite(t, page, redirectTo(bulkLocation), serve(torrent))
	p := &plugin{sessions: forumcommon.New(), transport: rec}

	payload, err := p.Download(context.Background(), &domain.Topic{URL: "https://nnmclub.to/forum/viewtopic.php?t=420880"}, nil, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(payload.TorrentFile, torrent) {
		t.Errorf("torrent file = %d bytes, want the served %d", len(payload.TorrentFile), len(torrent))
	}
	// Exactly one of the two: a client resolves the magnet first when both
	// are set, and the file would be thrown away.
	if payload.MagnetURI != "" {
		t.Errorf("magnet = %q, want none next to a torrent file", payload.MagnetURI)
	}
	if payload.FileName != "nnmclub-379398.torrent" {
		t.Errorf("file name = %q", payload.FileName)
	}
	if want := []string{"nnmclub.to", "nnmclub.to", "bulk.nnmclub.to"}; !reflect.DeepEqual(rec.hosts, want) {
		t.Errorf("request hosts = %v, want %v", rec.hosts, want)
	}
	if want := []string{"t=420880", "id=379398", "csid=d02f911f66e90b47&uid=-1&id=379398"}; !reflect.DeepEqual(rec.queries, want) {
		t.Errorf("request queries = %v, want %v", rec.queries, want)
	}
}

// An admin's active mirror gets the download request, and its own download
// host is as acceptable as the canonical one's.
func TestDownload_TorrentFileOnActiveMirror(t *testing.T) {
	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == pluginName {
			return registry.DomainConfig{Active: "nnmclub.mirror", Custom: []string{"nnmclub.mirror"}}
		}
		return registry.DomainConfig{}
	})
	t.Cleanup(func() { registry.SetDomainResolver(nil) })

	torrent, _, page := testTorrent(t)
	rec := downloadSite(t, page, redirectTo("//bulk.nnmclub.mirror/download.php?csid=1&uid=-1&id=379398"), serve(torrent))
	p := &plugin{sessions: forumcommon.New(), transport: rec}

	payload, err := p.Download(context.Background(), &domain.Topic{URL: testTopicURL}, nil, nil)
	if err != nil || !bytes.Equal(payload.TorrentFile, torrent) {
		t.Fatalf("Download = %+v, %v; want the torrent file", payload, err)
	}
	if want := []string{"nnmclub.mirror", "nnmclub.mirror", "bulk.nnmclub.mirror"}; !reflect.DeepEqual(rec.hosts, want) {
		t.Errorf("request hosts = %v, want %v", rec.hosts, want)
	}
}

// Every way the file can be missing or wrong must deliver the magnet: the
// download is a bonus on top of a link the page always carries.
func TestDownload_FallsBackToMagnet(t *testing.T) {
	_, hash, page := testTorrent(t)
	other := torrentmetatest.SingleFile("another-release.mkv", 700)

	for _, tc := range []struct {
		name  string
		page  string
		forum http.HandlerFunc
		bulk  http.HandlerFunc
		// hosts the plugin may reach, in order.
		wantHosts []string
	}{
		{
			name:      "no download link on the page",
			page:      strings.Replace(page, `<a href="download.php?id=379398" rel="nofollow">Скачать</a>`, "", 1),
			wantHosts: []string{"nnmclub.to"},
		},
		{
			name:      "download gated again: redirect to the login page",
			page:      page,
			forum:     redirectTo("login.php?redirect=download.php&id=379398"),
			wantHosts: []string{"nnmclub.to", "nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "200 with an HTML page",
			page:      page,
			forum:     serve([]byte("<html><body>Войдите, чтобы скачать</body></html>")),
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "a torrent other than the one the page names",
			page:      page,
			forum:     redirectTo(bulkLocation),
			bulk:      serve(other),
			wantHosts: []string{"nnmclub.to", "nnmclub.to", "bulk.nnmclub.to"},
		},
		{
			name:      "download host answers 404",
			page:      page,
			forum:     redirectTo(bulkLocation),
			bulk:      http.NotFound,
			wantHosts: []string{"nnmclub.to", "nnmclub.to", "bulk.nnmclub.to"},
		},
		{
			name:      "redirect to another site",
			page:      page,
			forum:     redirectTo("https://evil.example/download.php?id=379398"),
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "redirect to an internal address",
			page:      page,
			forum:     redirectTo("http://169.254.169.254/latest/meta-data/"),
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "redirect to a look-alike host",
			page:      page,
			forum:     redirectTo("//bulk.nnmclub.to.evil.example/download.php?id=379398"),
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "redirect to another subdomain",
			page:      page,
			forum:     redirectTo("//internal.nnmclub.to/download.php?id=379398"),
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "redirect to another port of the download host",
			page:      page,
			forum:     redirectTo("//bulk.nnmclub.to:6379/download.php?id=379398"),
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "redirect to plain http",
			page:      page,
			forum:     redirectTo("http://bulk.nnmclub.to/download.php?id=379398"),
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "redirect without a Location",
			page:      page,
			forum:     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusFound) },
			wantHosts: []string{"nnmclub.to", "nnmclub.to"},
		},
		{
			name:      "redirect loop",
			page:      page,
			forum:     redirectTo(bulkLocation),
			bulk:      redirectTo(bulkLocation),
			wantHosts: []string{"nnmclub.to", "nnmclub.to", "bulk.nnmclub.to", "bulk.nnmclub.to"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forum, bulk := tc.forum, tc.bulk
			if forum == nil {
				forum = unreachable(t, "download.php")
			}
			if bulk == nil {
				bulk = unreachable(t, "the download host")
			}
			rec := downloadSite(t, tc.page, forum, bulk)
			p := &plugin{sessions: forumcommon.New(), transport: rec}

			payload, err := p.Download(context.Background(), &domain.Topic{URL: "https://nnmclub.to/forum/viewtopic.php?t=420880"}, nil, nil)
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if len(payload.TorrentFile) != 0 {
				t.Errorf("got a %d-byte torrent file, want the magnet", len(payload.TorrentFile))
			}
			if want := "magnet:?xt=urn:btih:" + hash + "&dn="; !strings.HasPrefix(payload.MagnetURI, want) {
				t.Errorf("magnet = %q, want prefix %q", payload.MagnetURI, want)
			}
			if !reflect.DeepEqual(rec.hosts, tc.wantHosts) {
				t.Errorf("request hosts = %v, want %v", rec.hosts, tc.wantHosts)
			}
		})
	}
}

// The scheduler gives Download one budget for the page and the file. A
// download host that accepts the connection and never answers must not be
// able to spend it: the magnet needs no further request.
func TestDownload_StalledDownloadHostStillDeliversTheMagnet(t *testing.T) {
	_, hash, page := testTorrent(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rec := downloadSite(t, page, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}, unreachable(t, "the download host"))
	p := &plugin{sessions: forumcommon.New(), transport: rec, torrentTimeout: 50 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	payload, err := p.Download(ctx, &domain.Topic{URL: testTopicURL}, nil, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !strings.Contains(payload.MagnetURI, hash) {
		t.Errorf("magnet = %q, want the page's hash", payload.MagnetURI)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Download took %v: the stalled host spent the caller's budget", took)
	}
	if ctx.Err() != nil {
		t.Error("the caller's context expired while waiting for the download host")
	}
}

// The download host is a different host from the forum, and the clearance was
// minted for the forum. The request carries it there (as a browser would: the
// cookie is domain-wide), the topic page stays the only URL the solver is
// ever asked about, and a challenge on the download host does not drop the
// clearance that just read the topic page.
func TestDownload_ClearanceOnBothHops(t *testing.T) {
	torrent, hash, page := testTorrent(t)
	topic := "https://nnmclub.to/forum/viewtopic.php?t=420880"

	for _, tc := range []struct {
		name            string
		bulkChallenged  bool
		wantTorrentFile bool
	}{
		{"download host accepts the clearance", false, true},
		{"download host still challenges", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeClearanceProvider{c: testClearance()}
			withClearanceProvider(t, fp)
			var fetched []string
			p := &plugin{sessions: forumcommon.New(), transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				fetched = append(fetched, r.URL.Host+r.URL.Path)
				if !hasClearance(r, testClearance()) {
					t.Errorf("%s: missing clearance cookie/browser UA", r.URL)
				}
				switch r.URL.Host + r.URL.Path {
				case "nnmclub.to" + topicPath:
					return clearanceResponse(r, http.StatusOK, false, page), nil
				case "nnmclub.to" + downloadPath:
					resp := clearanceResponse(r, http.StatusFound, false, "")
					resp.Header.Set("Location", bulkLocation)
					return resp, nil
				case "bulk.nnmclub.to/download.php":
					if tc.bulkChallenged {
						return clearanceResponse(r, http.StatusForbidden, true, "Just a moment..."), nil
					}
					return clearanceResponse(r, http.StatusOK, false, string(torrent)), nil
				}
				t.Errorf("unexpected request: %s", r.URL)
				return clearanceResponse(r, http.StatusNotFound, false, ""), nil
			})}

			payload, err := p.Download(context.Background(), &domain.Topic{URL: topic}, nil, nil)
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if got := len(payload.TorrentFile) > 0; got != tc.wantTorrentFile {
				t.Errorf("torrent file delivered = %v, want %v (magnet %q)", got, tc.wantTorrentFile, payload.MagnetURI)
			}
			if !tc.wantTorrentFile && !strings.Contains(payload.MagnetURI, hash) {
				t.Errorf("magnet = %q, want the page's hash", payload.MagnetURI)
			}
			wantFetched := []string{"nnmclub.to" + topicPath, "nnmclub.to" + downloadPath, "bulk.nnmclub.to/download.php"}
			if !reflect.DeepEqual(fetched, wantFetched) {
				t.Errorf("requests = %v, want %v", fetched, wantFetched)
			}
			if want := []string{topic, topic, topic}; !reflect.DeepEqual(fp.probes, want) {
				t.Errorf("probes = %v, want only the topic page: a download URL is not a page the solver can open", fp.probes)
			}
			if len(fp.invalidated) != 0 {
				t.Errorf("invalidated = %v, want none: the clearance works for the topic page", fp.invalidated)
			}
		})
	}
}

func TestDownloadID(t *testing.T) {
	const magnet = `<a href="magnet:?xt=urn:btih:094EC3052ED759240E4DFD89F3F7CA5C5B428FF4">m</a>`
	for _, tc := range []struct {
		name, page, want string
	}{
		{"none", `<html>` + magnet + `</html>`, ""},
		{"link before the magnet", `<a href="download.php?id=379398">Скачать</a>` + magnet, "379398"},
		{"link after the magnet", magnet + `<a href="download.php?id=379398">Скачать</a>`, "379398"},
		{"absolute link", magnet + `<a href="https://nnmclub.to/forum/download.php?id=7">x</a>`, "7"},
		{
			"another release's link far from the magnet",
			`<a href="download.php?id=111">other</a>` + strings.Repeat("<p>reply</p>", 200) +
				`<a href="download.php?id=379398">Скачать</a>` + magnet,
			"379398",
		},
		{
			"another release's link after the magnet",
			`<a href="download.php?id=379398">Скачать</a>` + magnet + strings.Repeat("<p>reply</p>", 200) +
				`<a href="download.php?id=222">other</a>`,
			"379398",
		},
		{"no magnet: first link", `<a href="download.php?id=5">a</a><a href="download.php?id=6">b</a>`, "5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := downloadID([]byte(tc.page)); got != tc.want {
				t.Errorf("downloadID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTorrentHostAllowed(t *testing.T) {
	registry.SetDomainResolver(func(name string) registry.DomainConfig {
		if name == pluginName {
			return registry.DomainConfig{Custom: []string{"nnmclub.mirror"}}
		}
		return registry.DomainConfig{}
	})
	t.Cleanup(func() { registry.SetDomainResolver(nil) })

	for target, want := range map[string]bool{
		"https://nnmclub.to/forum/download.php?id=1":     true,
		"https://bulk.nnmclub.to/download.php?id=1":      true,
		"https://BULK.NNMCLUB.TO/download.php?id=1":      true,
		"https://bulk.nnmclub.mirror/download.php?id=1":  true,
		"https://bulk.bulk.nnmclub.to/download.php":      false,
		"https://dl.nnmclub.to/download.php":             false,
		"https://bulk.nnmclub.to.evil.example/x":         false,
		"https://bulk.evil.example/x":                    false,
		"https://bulk.nnmclub.to:8443/download.php":      false,
		"https://user:pass@bulk.nnmclub.to/download.php": false,
		"http://bulk.nnmclub.to/download.php":            false,
		"ftp://bulk.nnmclub.to/download.php":             false,
		"https://bulk./download.php":                     false,
		"https://127.0.0.1/download.php":                 false,
	} {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatalf("parse %q: %v", target, err)
		}
		if got := torrentHostAllowed(u); got != want {
			t.Errorf("torrentHostAllowed(%q) = %v, want %v", target, got, want)
		}
	}
}
