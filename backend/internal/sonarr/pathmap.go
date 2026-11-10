package sonarr

import (
	"errors"
	"net"
	"strings"
)

// errAmbiguousMapping is returned when Sonarr's remote path mappings do not
// give one answer for a path: two host-matched mappings with the same remote
// path but different local paths, or — when no mapping names the client's host —
// path-matching mappings that rewrite the path to different places.
var errAmbiguousMapping = errors.New("sonarr: ambiguous remote path mapping")

// osPath is a filesystem path from either side (the torrent client's or
// Sonarr's), normalised for comparison: forward slashes, no trailing slash
// except on a root ("/" or "C:/"). windows remembers the original style so the
// path can be handed back in the form its owner understands.
type osPath struct {
	p       string
	windows bool
}

// newOSPath normalises raw. A drive letter ("C:\…", "C:/…") or a UNC prefix
// ("\\server\…") marks it Windows-style, compared case-insensitively.
func newOSPath(raw string) osPath {
	o := osPath{windows: isWindowsPath(raw)}
	p := strings.ReplaceAll(raw, `\`, "/")
	for len(p) > 1 && strings.HasSuffix(p, "/") && !isDriveRoot(p) {
		p = p[:len(p)-1]
	}
	o.p = p
	return o
}

func isWindowsPath(raw string) bool {
	if strings.HasPrefix(raw, `\\`) {
		return true
	}
	return len(raw) >= 2 && raw[1] == ':' && isASCIILetter(raw[0])
}

func isDriveRoot(p string) bool {
	return len(p) == 3 && p[1] == ':' && p[2] == '/' && isASCIILetter(p[0])
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// join appends a client-relative file path ("Show/E01.mkv").
func (o osPath) join(rel string) osPath {
	rel = strings.Trim(strings.ReplaceAll(rel, `\`, "/"), "/")
	if rel == "" {
		return o
	}
	base := strings.TrimSuffix(o.p, "/")
	return osPath{p: base + "/" + rel, windows: o.windows}
}

// native renders the path in its owner's style.
func (o osPath) native() string {
	if o.windows {
		return strings.ReplaceAll(o.p, "/", `\`)
	}
	return o.p
}

// key is the comparison form. ASCII-only lowering keeps every byte offset
// valid, which mapPath relies on when it slices the original by a key length.
func (o osPath) key() string {
	if o.windows {
		return asciiLower(o.p)
	}
	return o.p
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// within reports whether key k equals prefix or lies under it, on a path
// segment boundary ("/data/tv" does not contain "/data/tvshows"). prefix
// carries no trailing slash; a root is passed as "" or "c:".
func within(k, prefix string) bool {
	return k == prefix || strings.HasPrefix(k, prefix+"/")
}

// mapPath translates a path the torrent client reports into the path Sonarr
// sees, using Sonarr's own remote path mappings — the same table Sonarr applies
// to paths its download clients report. Sonarr keys a mapping by the download
// client's host AND the remote path (RemotePathMappingService.RemapRemoteToLocal),
// and so does this, as far as Marauder can know the host.
//
// clientHost is the host of the delivering client as Marauder is configured to
// reach it. When one or more mappings name that host (case-insensitive,
// port-less), only those are considered and the longest remote path containing
// the client path wins, on a path-segment boundary; two of them with that same
// remote path but different local paths are ambiguous.
//
// When no mapping names the host — Sonarr may know the client by another name
// or IP — a mapping for another client must not be trusted on its path alone:
// the path is rewritten only if EVERY mapping whose remote path contains it, of
// any length and any host, rewrites it to the same place. Otherwise it is
// ambiguous and nothing is imported.
//
// No mapping containing the path at all returns it unchanged — the common setup
// where both see the same mount.
func mapPath(client osPath, clientHost string, mappings []RemotePathMapping) (osPath, bool, error) {
	host := normalizeHost(clientHost)
	var scoped []RemotePathMapping
	if host != "" {
		for _, m := range mappings {
			if normalizeHost(m.Host) == host {
				scoped = append(scoped, m)
			}
		}
	}
	if len(scoped) > 0 {
		return mapLongest(client, scoped)
	}
	return mapUnanimous(client, mappings)
}

// mapLongest applies the longest matching mapping among host-matched ones.
func mapLongest(client osPath, mappings []RemotePathMapping) (osPath, bool, error) {
	type match struct {
		path osPath
		n    int
	}
	var matches []match
	bestLen := -1
	for _, m := range mappings {
		if got, n, ok := applyMapping(client, m); ok {
			matches = append(matches, match{got, n})
			bestLen = max(bestLen, n)
		}
	}
	if len(matches) == 0 {
		return client, false, nil
	}
	var best *osPath
	for i := range matches {
		if matches[i].n != bestLen {
			continue
		}
		if best != nil && best.key() != matches[i].path.key() {
			return osPath{}, false, errAmbiguousMapping
		}
		best = &matches[i].path
	}
	return *best, true, nil
}

// mapUnanimous applies the mappings only when all that match agree.
func mapUnanimous(client osPath, mappings []RemotePathMapping) (osPath, bool, error) {
	var result osPath
	found := false
	for _, m := range mappings {
		got, _, ok := applyMapping(client, m)
		if !ok {
			continue
		}
		if found && got.key() != result.key() {
			return osPath{}, false, errAmbiguousMapping
		}
		result, found = got, true
	}
	if !found {
		return client, false, nil
	}
	return result, true, nil
}

// applyMapping rewrites client through one mapping, reporting the length of
// the matched remote prefix. A root remote path ("/", "C:/") trims to "" /
// "c:", so within() sees every prefix in the same no-trailing-slash form and
// the remainder always starts at a separator.
func applyMapping(client osPath, m RemotePathMapping) (osPath, int, bool) {
	if strings.TrimSpace(m.RemotePath) == "" || strings.TrimSpace(m.LocalPath) == "" {
		return osPath{}, 0, false
	}
	mk := strings.TrimSuffix(newOSPath(m.RemotePath).key(), "/")
	if !within(client.key(), mk) {
		return osPath{}, 0, false
	}
	local := newOSPath(m.LocalPath)
	p := strings.TrimSuffix(local.p, "/") + client.p[len(mk):] // byte offsets match key()
	switch {
	case p == "":
		p = "/"
	case len(p) == 2 && p[1] == ':':
		p += "/"
	}
	return osPath{p: p, windows: local.windows}, len(mk), true
}

// normalizeHost lower-cases a host and drops a port and IPv6 brackets, the
// form Sonarr compares mapping hosts in.
func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

// contentRoot is the part of the save path that holds exactly this torrent,
// derived from the client's own file list (paths relative to the save path):
// the single file of a single-file torrent, the shared top folder of a
// rooted torrent (as the client names it now, so a custom folder name is
// honoured), or the save path itself for a multi-file torrent without one.
func contentRoot(save osPath, files []string) osPath {
	if len(files) == 0 {
		return save
	}
	if len(files) == 1 && !strings.Contains(strings.Trim(files[0], "/"), "/") {
		return save.join(files[0])
	}
	var root string
	for i, f := range files {
		parts := strings.SplitN(strings.Trim(strings.ReplaceAll(f, `\`, "/"), "/"), "/", 2)
		if len(parts) < 2 {
			return save // a file directly in the save path: no shared folder
		}
		if i == 0 {
			root = parts[0]
		} else if parts[0] != root {
			return save
		}
	}
	return save.join(root)
}
