package sonarr

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"slices"
	"strings"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/extra"
)

// maxGrabHashes caps the Sonarr grab hashes remembered per topic. Sonarr can
// grab the same release more than once (a re-grab, another variant) before
// Marauder sees the first one complete; each grab adds up to two values (the
// indexer infohash and the client download id), so ten covers several grabs.
const maxGrabHashes = 10

// endpointFingerprint is a short, one-way fingerprint of a Sonarr base URL. A
// topic's instance+series link is bound to it: a series id is only meaningful
// on the Sonarr server that issued it, and an admin may re-point an instance
// at another server. The URL itself is not stored on the topic, because topic
// owners who are not admins can read a topic's extra.
//
// Normalised before hashing: scheme and host lower-cased, userinfo and query
// dropped, trailing slashes trimmed — so cosmetic edits do not invalidate the
// link. The API key is not part of it, so rotating the key keeps the link.
//
// The path is taken ESCAPED, as the client sends it (NewClient keeps the raw
// base URL and only trims literal trailing slashes): url.URL.Path decodes
// %2F, which would make https://proxy/sonarr%2Fblue and
// https://proxy/sonarr/blue — two different request targets — fingerprint
// alike and keep a stale series link alive across that edit.
func endpointFingerprint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	norm := strings.TrimRight(strings.ToLower(raw), "/")
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		norm = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:8])
}

// recordGrabHashes returns the hashes a grab identifies Sonarr's own torrent
// by: the indexer's torrentInfoHash and the history downloadId (a torrent
// client's infohash), de-duplicated case-insensitively. When the poller folded
// several grabs of the same URL into rec (batchGrabHashes), their hashes come
// first, so every grab in the batch is recognised, not only the newest.
func recordGrabHashes(rec HistoryRecord) []string {
	return appendGrabHashes(rec.batchGrabHashes, []string{rec.Data.TorrentInfoHash, rec.DownloadID})
}

// batchGrabHashes is the bounded union of the grab hashes of every record in
// recs (one URL's grabs from one poll), oldest first so the newest survive
// the cap.
func batchGrabHashes(recs []HistoryRecord) []string {
	sorted := slices.Clone(recs)
	slices.SortFunc(sorted, func(a, b HistoryRecord) int {
		if order := a.Date.Compare(b.Date); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var out []string
	for _, rec := range sorted {
		out = appendGrabHashes(out, []string{rec.Data.TorrentInfoHash, rec.DownloadID})
	}
	return out
}

// storedGrabHashes reads a topic's grab-hash set. A topic written before the
// set existed has only the single-value keys; those seed it, so its earlier
// grab is still recognised after the merge overwrites them.
func storedGrabHashes(topicExtra map[string]any) []string {
	set := extra.StringSlice(topicExtra, domain.TopicExtraSonarrGrabHashes)
	return appendGrabHashes(set, []string{
		extra.String(topicExtra, domain.TopicExtraSonarrInfoHash, ""),
		extra.String(topicExtra, domain.TopicExtraSonarrDownloadID, ""),
	})
}

// appendGrabHashes appends each non-empty hash, moving one already present
// (case-insensitively) to the end, and keeps only the newest maxGrabHashes.
func appendGrabHashes(set []string, hashes []string) []string {
	out := make([]string, 0, len(set)+len(hashes))
	for _, h := range set {
		out = appendHash(out, h)
	}
	for _, h := range hashes {
		out = appendHash(out, h)
	}
	if len(out) > maxGrabHashes {
		out = out[len(out)-maxGrabHashes:]
	}
	return out
}

func appendHash(set []string, h string) []string {
	h = strings.TrimSpace(h)
	if h == "" {
		return set
	}
	for i, existing := range set {
		if strings.EqualFold(existing, h) {
			set = append(set[:i], set[i+1:]...)
			break
		}
	}
	return append(set, h)
}

// sameHashes reports whether two sets hold the same hashes in the same order.
func sameHashes(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
