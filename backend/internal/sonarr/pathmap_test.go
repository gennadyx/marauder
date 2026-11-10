package sonarr

import (
	"errors"
	"testing"
)

func TestMapPath(t *testing.T) {
	// Client A maps /downloads, client B maps the deeper /downloads/tv.
	ab := []RemotePathMapping{
		{Host: "qbit-a", RemotePath: "/downloads/", LocalPath: "/mnt/a/"},
		{Host: "qbit-b", RemotePath: "/downloads/tv/", LocalPath: "/mnt/b/"},
	}
	tests := []struct {
		name     string
		in       string
		host     string
		mappings []RemotePathMapping
		want     string
		mapped   bool
		err      error
	}{
		{name: "no mappings passes through", in: "/downloads/tv/Show", want: "/downloads/tv/Show"},
		{
			name:     "host-matched mapping applied",
			in:       "/downloads/tv/Show",
			host:     "qbit",
			mappings: []RemotePathMapping{{Host: "QBit", RemotePath: "/downloads/", LocalPath: "/data/dl/"}},
			want:     "/data/dl/tv/Show",
			mapped:   true,
		},
		{
			name:     "single mapping, host unknown to Sonarr, applied unanimously",
			in:       "/downloads/tv/Show",
			host:     "transmission",
			mappings: []RemotePathMapping{{Host: "192.168.1.5", RemotePath: "/downloads/", LocalPath: "/data/dl/"}},
			want:     "/data/dl/tv/Show",
			mapped:   true,
		},
		{
			name: "longest remote path wins among the client's own mappings",
			in:   "/downloads/tv/Show",
			host: "qbit",
			mappings: []RemotePathMapping{
				{Host: "qbit", RemotePath: "/downloads", LocalPath: "/x"},
				{Host: "qbit", RemotePath: "/downloads/tv", LocalPath: "/y/tv"},
			},
			want:   "/y/tv/Show",
			mapped: true,
		},
		{
			name:     "regression: another client's deeper mapping is never used",
			in:       "/downloads/tv/E01.mkv",
			host:     "qbit-a",
			mappings: ab,
			want:     "/mnt/a/tv/E01.mkv",
			mapped:   true,
		},
		{
			name:     "regression: host unknown and overlapping mappings disagree",
			in:       "/downloads/tv/E01.mkv",
			host:     "transmission",
			mappings: ab,
			err:      errAmbiguousMapping,
		},
		{
			name:     "regression: no host and overlapping mappings disagree",
			in:       "/downloads/tv/E01.mkv",
			mappings: ab,
			err:      errAmbiguousMapping,
		},
		{
			name: "host unknown, overlapping mappings agree",
			in:   "/downloads/tv/E01.mkv",
			host: "transmission",
			mappings: []RemotePathMapping{
				{Host: "a", RemotePath: "/downloads/", LocalPath: "/mnt/dl/"},
				{Host: "b", RemotePath: "/downloads/tv/", LocalPath: "/mnt/dl/tv/"},
			},
			want:   "/mnt/dl/tv/E01.mkv",
			mapped: true,
		},
		{
			name: "the client's own mappings miss: another client's is not borrowed",
			in:   "/downloads/E01.mkv",
			host: "qbit-a",
			mappings: []RemotePathMapping{
				{Host: "qbit-a", RemotePath: "/other/", LocalPath: "/x/"},
				{Host: "qbit-b", RemotePath: "/downloads/", LocalPath: "/mnt/b/"},
			},
			want: "/downloads/E01.mkv",
		},
		{
			name:     "segment boundary respected",
			in:       "/downloads/tvshows/Show",
			mappings: []RemotePathMapping{{RemotePath: "/downloads/tv", LocalPath: "/y"}},
			want:     "/downloads/tvshows/Show",
		},
		{
			name:     "exact match maps the root itself",
			in:       "/downloads",
			mappings: []RemotePathMapping{{RemotePath: "/downloads/", LocalPath: "/data"}},
			want:     "/data",
			mapped:   true,
		},
		{
			name:     "windows client to linux sonarr, case-insensitive",
			in:       `D:\Torrents\TV\Show`,
			mappings: []RemotePathMapping{{RemotePath: `d:\torrents\`, LocalPath: "/mnt/torrents/"}},
			want:     "/mnt/torrents/TV/Show",
			mapped:   true,
		},
		{
			name:     "linux client to windows sonarr renders backslashes",
			in:       "/downloads/Show/E01.mkv",
			mappings: []RemotePathMapping{{RemotePath: "/downloads/", LocalPath: `\\nas\downloads\`}},
			want:     `\\nas\downloads\Show\E01.mkv`,
			mapped:   true,
		},
		{
			name: "same host, same remote path, different local paths is ambiguous",
			in:   "/downloads/Show",
			host: "qbit",
			mappings: []RemotePathMapping{
				{Host: "qbit", RemotePath: "/downloads/", LocalPath: "/one"},
				{Host: "qbit", RemotePath: "/downloads", LocalPath: "/two"},
			},
			err: errAmbiguousMapping,
		},
		{
			name: "two hosts, same remote and local path is fine",
			in:   "/downloads/Show",
			mappings: []RemotePathMapping{
				{Host: "a", RemotePath: "/downloads/", LocalPath: "/one/"},
				{Host: "b", RemotePath: "/downloads", LocalPath: "/one"},
			},
			want:   "/one/Show",
			mapped: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, mapped, err := mapPath(newOSPath(tt.in), tt.host, tt.mappings)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			if got.native() != tt.want || mapped != tt.mapped {
				t.Errorf("mapPath(%q) = %q mapped=%v, want %q mapped=%v", tt.in, got.native(), mapped, tt.want, tt.mapped)
			}
		})
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"QBittorrent":      "qbittorrent",
		" qbit:8080 ":      "qbit",
		"192.168.1.5:9091": "192.168.1.5",
		"[::1]:8080":       "::1",
		"[::1]":            "::1",
		"":                 "",
	} {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentRoot(t *testing.T) {
	save := newOSPath("/downloads/tv/")
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"single file", []string{"Show.S01E01.mkv"}, "/downloads/tv/Show.S01E01.mkv"},
		{"rooted", []string{"Show S01/E01.mkv", "Show S01/E02.mkv"}, "/downloads/tv/Show S01"},
		{"renamed root is whatever the client reports", []string{"My Name/E01.mkv"}, "/downloads/tv/My Name"},
		{"no root folder", []string{"E01.mkv", "E02.mkv"}, "/downloads/tv"},
		{"mixed roots", []string{"A/E01.mkv", "B/E02.mkv"}, "/downloads/tv"},
		{"file beside a folder", []string{"A/E01.mkv", "E02.mkv"}, "/downloads/tv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contentRoot(save, tt.files).native(); got != tt.want {
				t.Errorf("contentRoot = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOSPathKey_WindowsCaseInsensitive(t *testing.T) {
	if newOSPath(`C:\TV\Show.mkv`).key() != newOSPath(`c:/tv/show.mkv`).key() {
		t.Error("windows paths must compare case- and separator-insensitively")
	}
	if newOSPath("/TV/Show.mkv").key() == newOSPath("/tv/show.mkv").key() {
		t.Error("posix paths must compare case-sensitively")
	}
}
