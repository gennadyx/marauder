// Package nnmclub implements the NNM-Club.to tracker plugin.
//
// NNM-Club is the third of the "big three" CIS forum trackers Marauder
// targets. The site sits behind Cloudflare; its login is
// Turnstile-gated, so the plugin is anonymous-only.
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
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	"github.com/artyomsv/marauder/backend/internal/plugins/trackers/forumcommon"
)

const (
	pluginName  = "nnmclub"
	displayName = "NNM-Club.to"
	userAgent   = "Marauder/0.3 (+https://marauder.cc)"
)

// knownDomains deliberately no longer lists nnmclub.me: as of 2026-08-03 it
// is a parked domain (it serves a router.parklogic.com redirect page, not the
// forum). Leaving it here would let domain rotation move a working install
// onto a parking page it can never recover from on its own.
var knownDomains = []string{"nnmclub.to"}

type plugin struct {
	sessions  *forumcommon.SessionStore
	transport http.RoundTripper
}

func init() {
	registry.RegisterTracker(&plugin{
		sessions: forumcommon.New(),
	})
}

func (p *plugin) Name() string        { return pluginName }
func (p *plugin) DisplayName() string { return displayName }

// UsesCloudflare implements registry.WithCloudflare.
func (p *plugin) UsesCloudflare() bool { return true }

var _ registry.WithDomains = (*plugin)(nil)

// Domains implements registry.WithDomains; first entry is canonical.
func (p *plugin) Domains() []string { return knownDomains }

var _ registry.WithCanonicalParseURL = (*plugin)(nil)

func (p *plugin) CanonicalizesTopicURL() bool { return true }

func (p *plugin) CanParse(rawURL string) bool {
	_, err := parseTopicURL(rawURL)
	return err == nil
}

// postResolveTimeout bounds the optional lookup: both HTTP and headless
// creation call Parse without a separate parse budget. Cloudflare, a timeout
// or missing canonical markup must never prevent an add.
const postResolveTimeout = 5 * time.Second

func (p *plugin) Parse(ctx context.Context, rawURL string) (*domain.Topic, error) {
	ref, err := parseTopicURL(rawURL)
	if err != nil {
		return nil, err
	}
	if ref.param == "p" {
		rctx, cancel := context.WithTimeout(ctx, postResolveTimeout)
		target := ref.active().String()
		body, ferr := p.fetch(rctx, target, nil)
		cancel()
		if ferr == nil {
			if id := topicIDFromPage(body, target); id != 0 {
				ref.param, ref.id = "t", id
			}
		}
	}
	// Stored identity always uses the default domain; active mirrors are a
	// fetch-time concern. Strip fragments, pagination and session parameters.
	ref.host = knownDomains[0]
	kind := "topic"
	if ref.param == "p" {
		kind = "post"
	}
	return &domain.Topic{
		TrackerName: pluginName,
		URL:         ref.String(),
		DisplayName: fmt.Sprintf("NNM-Club %s %d", kind, ref.id),
		Extra:       map[string]any{kind + "_id": ref.id},
	}, nil
}

// NNM-Club is anonymous-only in Marauder: its login is gated by Cloudflare
// Turnstile, which blocks automated (headless/server-side) sign-in, so the
// plugin deliberately does NOT implement registry.WithCredentials. Delivery
// uses the public, account-free magnet on the topic page.

// --- Check / Download --------------------------------------------------

var (
	titleRe  = regexp.MustCompile(`(?s)<title>([^<]+)</title>`)
	magnetRe = regexp.MustCompile(`magnet:\?xt=urn:btih:([A-Fa-f0-9]{40})`)
	// ogImageRe expects property before content (the order NNM-Club emits).
	// If that order ever reverses it fails open: ImageURL stays empty, no error.
	ogImageRe = regexp.MustCompile(`(?i)<meta[^>]+property=["']og:image["'][^>]+content=["']([^"']+)["']`)
)

func (p *plugin) Check(ctx context.Context, topic *domain.Topic, creds *domain.TrackerCredential) (*domain.Check, error) {
	body, _, err := p.fetchTopicPage(ctx, topic.URL, creds)
	if err != nil {
		return nil, err
	}
	check := &domain.Check{}
	if m := titleRe.FindSubmatch(body); m != nil {
		check.DisplayName = forumcommon.CleanTitle(string(m[1]), " :: NNM-Club")
	}
	if m := magnetRe.FindSubmatch(body); m != nil {
		check.Hash = strings.ToLower(string(m[1]))
	} else {
		return nil, errors.New("nnm-club: no infohash found")
	}
	return check, nil
}

// Download returns a magnet for the topic. Anonymous (no creds) is the only
// supported mode in Phase 1: the .torrent at download.php is login-gated
// (302 -> login), and a fresh anonymous fetch of the topic page yields a
// hash-only magnet (NNM-Club only embeds its tracker announce in the magnet
// for logged-in sessions). So we return the page's hash-only magnet enriched
// with a real &dn= display name. Peer discovery relies on DHT and may stall on
// poorly-seeded torrents — the reliable fix is the credentialed .torrent (with
// the user's passkey'd announce, also crediting ratio), Phase 2.
func (p *plugin) Download(ctx context.Context, topic *domain.Topic, _ *domain.Check, creds *domain.TrackerCredential) (*domain.Payload, error) {
	body, _, err := p.fetchTopicPage(ctx, topic.URL, creds)
	if err != nil {
		return nil, err
	}
	m := magnetRe.FindSubmatch(body)
	if m == nil {
		return nil, errors.New("nnm-club: topic page has no magnet link")
	}
	magnet := "magnet:?xt=urn:btih:" + strings.ToLower(string(m[1]))
	if mt := titleRe.FindSubmatch(body); mt != nil {
		if name := forumcommon.CleanTitle(string(mt[1]), " :: NNM-Club"); name != "" {
			magnet += "&dn=" + url.QueryEscape(name)
		}
	}
	return &domain.Payload{MagnetURI: magnet}, nil
}

// ResolveMetadata implements registry.WithMetadata: it fetches the topic page
// anonymously and extracts a human title + poster (og:image) for the AddTopic
// preview card.
var _ registry.WithMetadata = (*plugin)(nil)

func (p *plugin) ResolveMetadata(ctx context.Context, rawURL string, creds *domain.TrackerCredential) (*registry.Metadata, error) {
	body, _, err := p.fetchTopicPage(ctx, rawURL, creds)
	if err != nil {
		return nil, fmt.Errorf("nnm-club resolve metadata: %w", err)
	}
	meta := &registry.Metadata{}
	if m := titleRe.FindSubmatch(body); m != nil {
		meta.Title = forumcommon.CleanTitle(string(m[1]), " :: NNM-Club")
	}
	if m := ogImageRe.FindSubmatch(body); m != nil {
		meta.ImageURL = strings.TrimSpace(string(m[1]))
	}
	return meta, nil
}

func (p *plugin) fetch(ctx context.Context, target string, creds *domain.TrackerCredential) ([]byte, error) {
	// SSRF guard: `target` is a user-supplied topic URL. Parse it and confine
	// the request to NNM-Club's own hosts before dialing — an off-site URL
	// could otherwise point the backend at an internal service (cloud
	// metadata, localhost). The request is built from the parsed, host-checked
	// URL (u) so the value actually dialed is the one that passed the guard.
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil {
		return nil, fmt.Errorf("nnm-club: invalid URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("nnm-club: refusing URL scheme %q", u.Scheme)
	}
	// Allowlist the host against known NNM-Club domains plus any
	// admin-configured custom mirrors (registry.DomainAllowed). This is
	// still a strict allowlist — the CodeQL go/request-forgery alert on
	// this fetch is already dismissed won't-fix (CodeQL can't model the
	// runtime resolver), so the barrier stays the same shape, just backed
	// by a runtime-configurable set instead of compile-time literals.
	if !registry.DomainAllowed(pluginName, u.Hostname(), knownDomains) {
		return nil, fmt.Errorf("nnm-club: refusing to fetch off-site host %q", u.Hostname())
	}

	key := pluginName + ":nocreds"
	if creds != nil {
		key = forumcommon.SessionKey(pluginName, creds.UserID.String())
	}
	sess := p.sessions.GetOrCreate(key, userAgent)
	if p.transport != nil {
		sess.Client.Transport = p.transport
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := sess.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("nnm-club GET %s -> %d", target, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
