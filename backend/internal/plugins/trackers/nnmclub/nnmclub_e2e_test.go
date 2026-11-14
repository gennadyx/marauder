package nnmclub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/artyomsv/marauder/backend/internal/plugins/e2etest"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	"github.com/artyomsv/marauder/backend/internal/plugins/trackers/forumcommon"
)

func TestE2E(t *testing.T) {
	torrent, hash, page := testTorrent(t)
	e2etest.RunFullPipeline(t, e2etest.Case{
		Name: "nnmclub/anonymous-torrent-then-qbit",
		Setup: func(t *testing.T, _ *e2etest.QBitFake) (registry.Tracker, string) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/forum/viewtopic.php"):
					w.WriteHeader(200)
					_, _ = w.Write([]byte(page))
				// The two download hops, as the site answers a guest: the
				// forum redirects to the download host, which serves the file.
				case r.URL.Path == downloadPath:
					w.Header().Set("Location", bulkLocation)
					w.WriteHeader(http.StatusFound)
				case r.URL.Path == "/download.php":
					w.Header().Set("Content-Type", "application/x-bittorrent")
					_, _ = w.Write(torrent)
				case strings.HasPrefix(r.URL.Path, "/forum/login.php"):
					w.WriteHeader(200)
					_, _ = w.Write([]byte(`<a href="logout.php">logout</a>`))
				case r.URL.Path == "/forum/index.php":
					w.WriteHeader(200)
					_, _ = w.Write([]byte(`<a href="logout.php">logout</a>`))
				default:
					w.WriteHeader(404)
				}
			}))
			t.Cleanup(srv.Close)

			testHost := strings.TrimPrefix(srv.URL, "http://")
			p := &plugin{
				sessions: forumcommon.New(),
				transport: &e2etest.HostRewriteTransport{
					From: "nnmclub.to",
					To:   testHost,
					// The download host is bulk.nnmclub.to.
					StripSubdomain: true,
				},
			}
			return p, "https://nnmclub.to/forum/viewtopic.php?t=420880"
		},
		// No account: the real .torrent, not the page's hash-only magnet.
		ExpectedHash:      hash,
		ExpectTorrentFile: true,
	})
}
