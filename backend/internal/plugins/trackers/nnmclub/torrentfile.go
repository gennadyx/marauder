package nnmclub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/infohash"
	"github.com/artyomsv/marauder/backend/internal/plugins/cfclearance"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
)

// The account-free .torrent. Measured 2026-11-14 with a plain GET, no cookie
// and Marauder's own User-Agent, on a 2011 release and on one published that
// day: /forum/download.php?id=<n> answers a guest
//
//	302  Location: //bulk.nnmclub.to/download.php?csid=<hex>&uid=-1&id=<n>
//
// and that URL serves application/x-bittorrent. The file's infohash equals the
// magnet on the topic page, and it carries what the magnet lacks: the announce
// URLs (with a guest passkey), the comment (the topic's ?p= link) and the file
// list. The update policy and the folder name both need the file list. With a
// magnet, an update under the update policy is added paused; a stopped torrent
// fetches no metadata, so it stays in the client with no files, under the page
// title, and nothing rechecks the data already on disk.
//
// An earlier version of this plugin recorded download.php as login-gated
// (302 to the login page). The site may gate it again, so every failure here
// falls back to the magnet.

const (
	downloadPath = "/forum/download.php"
	// torrentHostPrefix is the download host's label in front of the forum's
	// domain. A redirect to any other host is refused.
	torrentHostPrefix = "bulk."
	// torrentFetchTimeout bounds both hops together. The scheduler gives
	// Download one budget for the page and the file, and the magnet fallback
	// needs none of it, so a stalled download host must not be able to spend
	// what is left.
	torrentFetchTimeout = 10 * time.Second
	// maxTorrentHops is the forum request plus the redirects followed.
	maxTorrentHops = 3
	maxTorrentSize = 8 << 20
)

var downloadLinkRe = regexp.MustCompile(`download\.php\?id=(\d+)`)

// downloadID returns the id of the download link nearest to the page's magnet
// link (they share a table cell), "" when the page has none. The pick is not
// trusted on its own: the fetched file must carry the magnet's infohash.
func downloadID(page []byte) string {
	links := downloadLinkRe.FindAllSubmatchIndex(page, -1)
	if len(links) == 0 {
		return ""
	}
	best := links[0]
	if magnet := magnetRe.FindIndex(page); magnet != nil {
		distance := func(link []int) int {
			if d := link[0] - magnet[0]; d >= 0 {
				return d
			}
			return magnet[0] - link[0]
		}
		for _, link := range links[1:] {
			if distance(link) < distance(best) {
				best = link
			}
		}
	}
	return string(page[best[2]:best[3]])
}

// torrentHostAllowed confines a .torrent request to the forum's own host or
// its download host. u.Host keeps any port, so an allowed name does not make
// an arbitrary port acceptable.
func torrentHostAllowed(u *url.URL) bool {
	if u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	if registry.DomainAllowed(pluginName, host, knownDomains) {
		return true
	}
	rest, ok := strings.CutPrefix(host, torrentHostPrefix)
	return ok && registry.DomainAllowed(pluginName, rest, knownDomains)
}

// fetchTorrentFile returns the topic's .torrent, or an error saying why the
// caller should deliver the magnet instead. page is the topic page, ref its
// URL on the active domain, want the infohash of the page's magnet.
func (p *plugin) fetchTorrentFile(ctx context.Context, page []byte, ref topicURL, want string, creds *domain.TrackerCredential) ([]byte, string, error) {
	id := downloadID(page)
	if id == "" {
		return nil, "", errors.New("the topic page has no download link")
	}
	budget := torrentFetchTimeout
	if p.torrentTimeout > 0 {
		budget = p.torrentTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	sess := p.session(creds)
	// A copy, so the redirect rule is this call's own: the stored client is
	// shared by every topic. The copy keeps the jar and the transport.
	client := *sess.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	u := &url.URL{Scheme: "https", Host: ref.host, Path: downloadPath, RawQuery: "id=" + id}
	for hop := 0; hop < maxTorrentHops; hop++ {
		// Every hop is checked, not just the first: the redirect target comes
		// from the site.
		if !torrentHostAllowed(u) {
			return nil, "", fmt.Errorf("refusing download host %q", u.Host)
		}
		body, next, err := p.torrentHop(ctx, &client, u, ref)
		if err != nil {
			return nil, "", err
		}
		if next != nil {
			u = next
			continue
		}
		got, err := infohash.FromTorrent(body)
		if err != nil {
			// A gated download answers 200 with an HTML page.
			return nil, "", fmt.Errorf("%s did not return a torrent: %w", u.Host, err)
		}
		if got != want {
			return nil, "", fmt.Errorf("torrent infohash %s does not match the topic magnet %s", got, want)
		}
		return body, "nnmclub-" + id + ".torrent", nil
	}
	return nil, "", errors.New("too many redirects")
}

// torrentHop makes one request and returns the body, or the redirect target.
// Errors name the host and path only: the download host's query carries a
// session id.
func (p *plugin) torrentHop(ctx context.Context, client *http.Client, u *url.URL, ref topicURL) ([]byte, *url.URL, error) {
	where := u.Host + u.Path
	// The topic page is the probe on both hosts. It was fetched a moment ago,
	// so its clearance is cached, and a download URL must never be handed to
	// the solver as a page to open. Seeding the forum's cookie onto the
	// download host is what a browser does too: Cloudflare scopes
	// cf_clearance to the whole domain. Whether the download host's rule
	// accepts it is not measured — from an unchallenged address neither host
	// asks — so a challenge here is a plain failure: the clearance that just
	// read the topic page is not dropped and re-minted for it.
	clearance, clearErr := cfclearance.Apply(ctx, pluginName, client.Jar, u, ref.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: invalid request", where)
	}
	req.Header.Set("User-Agent", cfclearance.UserAgent(clearance, userAgent))
	resp, err := client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // the wrapper's text is the full URL
		}
		return nil, nil, fmt.Errorf("GET %s: %w", where, err)
	}
	defer resp.Body.Close()
	switch {
	case cfclearance.IsChallenge(resp):
		return nil, nil, fmt.Errorf("GET %s: %w", where, cfclearance.Cause(clearErr))
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || resp.Header.Get("Location") == "" {
			return nil, nil, fmt.Errorf("GET %s -> %d without a usable Location", where, resp.StatusCode)
		}
		return nil, u.ResolveReference(loc), nil
	case resp.StatusCode != http.StatusOK:
		return nil, nil, fmt.Errorf("GET %s -> %d", where, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentSize+1))
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: read body: %w", where, err)
	}
	if len(body) > maxTorrentSize {
		return nil, nil, fmt.Errorf("GET %s: response larger than %d bytes", where, maxTorrentSize)
	}
	return body, nil, nil
}
