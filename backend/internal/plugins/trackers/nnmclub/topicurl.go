package nnmclub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
)

const topicPath = "/forum/viewtopic.php"

// topicURL keeps post and topic IDs distinct: phpBB's p= selects a post,
// possibly on a later page. It must never be substituted directly into t=.
type topicURL struct {
	host  string
	param string
	id    int
}

func parseTopicURL(rawURL string) (topicURL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return topicURL{}, fmt.Errorf("nnm-club: invalid URL: %w", err)
	}
	// Host deliberately includes any port, like the old regex capture: an
	// otherwise allowed hostname must not make an arbitrary port acceptable.
	if (u.Scheme != "https" && u.Scheme != "http") || u.User != nil ||
		u.Path != topicPath || u.RawPath != "" ||
		!registry.DomainAllowed(pluginName, u.Host, knownDomains) {
		return topicURL{}, errors.New("not a nnm-club viewtopic URL")
	}
	// Malformed extras must not invalidate a stored URL. ParseQuery still
	// returns the pairs it could decode when another pair contains ; or %.
	q, _ := url.ParseQuery(u.RawQuery)
	if values := q["p"]; len(values) == 1 && values[0] != "" && strings.Trim(values[0], "0123456789") == "" {
		// A single valid post selector takes priority; otherwise try t=.
		if id, err := strconv.Atoi(values[0]); err == nil && id > 0 {
			return topicURL{host: u.Host, param: "p", id: id}, nil
		}
	}
	value := q.Get("t") // Keep the first t=, even when later ones differ.
	if strings.HasPrefix(u.RawQuery, "t=") {
		// The old regex accepted any leading t=<digits>. Recover that first
		// pair even if ParseQuery dropped it for a semicolon or bad escape.
		pair, _, _ := strings.Cut(u.RawQuery, "&")
		rawValue := strings.TrimPrefix(pair, "t=")
		value, err = url.QueryUnescape(rawValue)
		if err != nil {
			value = rawValue
		}
	}
	n := 0
	for n < len(value) && value[n] >= '0' && value[n] <= '9' {
		n++
	}
	if n == 0 {
		return topicURL{}, errors.New("nnm-club: expected a numeric topic or post id")
	}
	// Preserve the old topic selector's leading-digit (including zero)
	// semantics; post IDs above deliberately require a whole positive ID.
	id, err := strconv.Atoi(value[:n])
	if err != nil {
		return topicURL{}, errors.New("nnm-club: topic or post id is out of range")
	}
	return topicURL{host: u.Host, param: "t", id: id}, nil
}

func (u topicURL) String() string {
	return fmt.Sprintf("https://%s%s?%s=%d", u.host, topicPath, u.param, u.id)
}

func (u topicURL) active() topicURL {
	if host := registry.ActiveDomain(pluginName); host != "" {
		u.host = host
	}
	return u
}

// topicIDFromPage reads only the head's canonical link, never arbitrary t=
// links (the navigation and replies link to other releases). Both a live p=
// page and a later t=&start= page exposed this marker on 2026-11-10; the latter
// includes start=, so only its topic ID is used. Tokenizing handles attribute
// order, quoting and HTML entities without depending on the page's charset.
func topicIDFromPage(body []byte, target string) int {
	base, err := url.Parse(target)
	if err != nil {
		return 0
	}
	z := html.NewTokenizer(bytes.NewReader(body))
	inHead := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return 0
		case html.EndTagToken:
			if z.Token().Data == "head" {
				return 0
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			if tok.Data == "body" {
				return 0
			}
			if tok.Data == "head" {
				inHead = true
			}
			if !inHead || tok.Data != "link" {
				continue
			}
			var rel, href string
			for _, attr := range tok.Attr {
				switch attr.Key {
				case "rel":
					rel = attr.Val
				case "href":
					href = attr.Val
				}
			}
			if !strings.EqualFold(strings.TrimSpace(rel), "canonical") {
				continue
			}
			link, err := url.Parse(strings.TrimSpace(href))
			if err != nil || href == "" {
				continue
			}
			ref, err := parseTopicURL(base.ResolveReference(link).String())
			if err == nil && ref.param == "t" {
				return ref.id
			}
		}
	}
}

// fetchTopicPage retries resolution for a stored p= fallback, then fetches
// page one explicitly. A later post page may lack the release magnet/poster;
// it is also unsafe to infer the release author from its first visible reply.
// If no topic ID is exposed, Check/Download/metadata can still use the p=
// page's own content, while AuthorComment must skip uncertain attribution.
func (p *plugin) fetchTopicPage(ctx context.Context, rawURL string, creds *domain.TrackerCredential) ([]byte, topicURL, error) {
	ref, err := parseTopicURL(rawURL)
	if err != nil {
		return nil, topicURL{}, err
	}
	ref = ref.active()
	target := ref.String()
	body, err := p.fetch(ctx, target, creds)
	if err != nil {
		return nil, ref, err
	}
	if ref.param == "p" {
		if id := topicIDFromPage(body, target); id != 0 {
			ref.param, ref.id = "t", id
			body, err = p.fetch(ctx, ref.String(), creds)
		}
	}
	return body, ref, err
}
