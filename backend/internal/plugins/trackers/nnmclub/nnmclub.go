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

	"github.com/rs/zerolog/log"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/plugins/cfclearance"
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
	// torrentTimeout overrides torrentFetchTimeout. Zero means the constant;
	// only tests set it, so a stalled-host test need not wait the real budget.
	torrentTimeout time.Duration
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

var _ registry.WithChallengeProbe = (*plugin)(nil)

// ChallengeProbeURL joins the backend's boot warm-up. This topic answered
// 403 Cf-Mitigated on the owner's server on 2026-11-11; the root is not a
// proven probe. Clearances are cached per host but satisfy the rule that
// issued them, so on-demand fetches probe their own requested page instead.
func (p *plugin) ChallengeProbeURL() string {
	return (topicURL{host: knownDomains[0], param: "t", id: 420880}).active().String()
}

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
// or missing canonical markup must never prevent an add. A warm clearance
// is reusable within this budget; a cold mint is refused by FlareSolverr's
// solveBudget before contacting the solver (normally a 20s minimum).
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
// uses the account-free .torrent, and the page's public magnet when that
// cannot be fetched.

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

// Download returns the topic's .torrent, which the site serves without an
// account (see torrentfile.go), and falls back to the page's magnet when the
// file cannot be fetched or is not the torrent the page names.
//
// The magnet is the authority for what the topic is — Check derives the topic
// hash from it — so the file is accepted only with that exact infohash. The
// fallback magnet is hash-only (the page embeds no announce URL for a guest)
// with a &dn= display name: peers come from DHT alone, and the update policy
// and the folder name cannot use it. That is a real downgrade, so it is
// logged at Warn rather than taken silently.
func (p *plugin) Download(ctx context.Context, topic *domain.Topic, _ *domain.Check, creds *domain.TrackerCredential) (*domain.Payload, error) {
	body, ref, err := p.fetchTopicPage(ctx, topic.URL, creds)
	if err != nil {
		return nil, err
	}
	m := magnetRe.FindSubmatch(body)
	if m == nil {
		return nil, errors.New("nnm-club: topic page has no magnet link")
	}
	hash := strings.ToLower(string(m[1]))
	file, fileName, ferr := p.fetchTorrentFile(ctx, body, ref, hash, creds)
	if ferr == nil {
		// Exactly one of TorrentFile and MagnetURI: every client resolves a
		// magnet first when both are set, which would throw the file away.
		return &domain.Payload{TorrentFile: file, FileName: fileName}, nil
	}
	log.Warn().Str("tracker", pluginName).Str("url", ref.String()).Err(ferr).
		Msg("nnm-club: could not fetch the .torrent; delivering the magnet instead")
	magnet := "magnet:?xt=urn:btih:" + hash
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

// session returns the shared HTTP session the plugin's requests run on.
func (p *plugin) session(creds *domain.TrackerCredential) *forumcommon.Session {
	key := pluginName + ":nocreds"
	if creds != nil {
		key = forumcommon.SessionKey(pluginName, creds.UserID.String())
	}
	sess := p.sessions.GetOrCreate(key, userAgent)
	if p.transport != nil {
		sess.Client.Transport = p.transport
	}
	return sess
}

func (p *plugin) fetch(ctx context.Context, target string, creds *domain.TrackerCredential) ([]byte, error) {
	return cfclearance.RetryOnChallenge(target, func() ([]byte, error) {
		return p.fetchOnce(ctx, target, creds)
	})
}

func (p *plugin) fetchOnce(ctx context.Context, target string, creds *domain.TrackerCredential) ([]byte, error) {
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

	sess := p.session(creds)
	// Probe the guarded page itself: a root-issued clearance may not satisfy
	// viewtopic's rule. Apply reuses the per-host cache before any cold mint,
	// so Parse's short budget can use the clearance earned by boot warm-up.
	clearance, clearErr := cfclearance.Apply(ctx, pluginName, sess.Client.Jar, u, u.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", cfclearance.UserAgent(clearance, userAgent))
	resp, err := sess.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// A managed challenge is a block, not a rejected login. Preserve the
	// provider's cause before the ordinary HTTP-status error is considered.
	if cfclearance.IsChallenge(resp) {
		return nil, fmt.Errorf("nnm-club GET %s: %w", target, cfclearance.Cause(clearErr))
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("nnm-club GET %s -> %d", target, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
