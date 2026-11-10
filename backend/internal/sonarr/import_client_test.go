package sonarr

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManualImportCandidates_SendsFolderNeverSeriesID(t *testing.T) {
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/manualimport" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("api key header missing")
		}
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`[{"path":"/tv/Show/E01.mkv","folderName":"Show","series":null,"episodes":[],` +
			`"quality":{"quality":{"id":3}},"languages":[{"id":1}],"rejections":[{"reason":"Unknown Series","type":"permanent"}]}]`))
	}))
	defer srv.Close()

	items, err := NewClient(srv.URL, "k", 5*time.Second).ManualImportCandidates(context.Background(), "/tv/Show")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery["folder"][0] != "/tv/Show" || gotQuery["filterExistingFiles"][0] != "true" {
		t.Errorf("query = %v", gotQuery)
	}
	// Released v3/v4 answer a seriesId request with the series' library files.
	if _, ok := gotQuery["seriesId"]; ok {
		t.Error("seriesId must never be sent to GET manualimport")
	}
	if len(items) != 1 || items[0].Series != nil || items[0].Rejections[0].Reason != "Unknown Series" {
		t.Errorf("items = %+v", items)
	}
	if string(items[0].Quality) != `{"quality":{"id":3}}` {
		t.Errorf("quality must be carried verbatim, got %s", items[0].Quality)
	}
}

func TestReprocessManualImport_ForcesSeriesAndNeverNullLanguages(t *testing.T) {
	var body []map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/manualimport" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`[{"path":"/tv/Show/E01.mkv","seriesId":42,"episodes":[{"id":7,"hasFile":true,"episodeFileId":31}],` +
			`"quality":{"quality":{"id":3}},"languages":[{"id":1},{"id":11}],"rejections":[]}]`))
	}))
	defer srv.Close()

	out, err := NewClient(srv.URL, "k", 5*time.Second).ReprocessManualImport(context.Background(), 42, []ManualImportItem{
		// Two scanned languages: v4.0.0.748 answers 500 if they are sent back.
		{Path: "/tv/Show/E01.mkv", Quality: json.RawMessage(`{"quality":{"id":3}}`),
			Languages: json.RawMessage(`[{"id":1},{"id":11}]`), Language: json.RawMessage(`{"id":1}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 {
		t.Fatalf("body = %v", body)
	}
	if string(body[0]["seriesId"]) != "42" || string(body[0]["episodeIds"]) != "[]" {
		t.Errorf("series/episodes = %s / %s", body[0]["seriesId"], body[0]["episodeIds"])
	}
	if _, ok := body[0]["seasonNumber"]; ok {
		t.Error("seasonNumber must be omitted so Sonarr parses the episodes itself")
	}
	if string(body[0]["languages"]) != "[]" {
		t.Errorf("languages must always be [] on reprocess (v4 dereferences it, v4.0.0.748 crashes on two), got %s", body[0]["languages"])
	}
	if string(body[0]["language"]) != `{"id":1}` {
		t.Errorf("v3 language not carried: %s", body[0]["language"])
	}
	if len(out) != 1 || out[0].SeriesID != 42 || len(out[0].Episodes) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if ep := out[0].Episodes[0]; !ep.HasFile || ep.EpisodeFileID != 31 {
		t.Errorf("episode library state not decoded: %+v", ep)
	}
	if string(out[0].Languages) != `[{"id":1},{"id":11}]` {
		t.Errorf("response languages must be kept for the command, got %s", out[0].Languages)
	}
}

func TestManualImport_CopyModeNoDownloadID(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/command" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"name":"ManualImport","status":"queued"}`))
	}))
	defer srv.Close()

	cmd, err := NewClient(srv.URL, "k", 5*time.Second).ManualImport(context.Background(), []ManualImportFile{
		{Path: "/tv/Show/E01.mkv", SeriesID: 42, EpisodeIDs: []int{7}, Quality: json.RawMessage(`{"quality":{"id":3}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.ID != 99 {
		t.Errorf("command id = %d", cmd.ID)
	}
	for _, want := range []string{`"name":"ManualImport"`, `"importMode":"copy"`, `"seriesId":42`, `"episodeIds":[7]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("body missing %s: %s", want, raw)
		}
	}
	if strings.Contains(raw, "downloadId") {
		t.Errorf("downloadId must never be sent: %s", raw)
	}
}

func TestManualImport_RefusesEmpty(t *testing.T) {
	if _, err := NewClient("http://sonarr.invalid", "k", time.Second).ManualImport(context.Background(), nil); err == nil {
		t.Fatal("an empty file list must not reach Sonarr")
	}
}

func TestRemotePathMappings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/remotepathmapping" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"id":1,"host":"qbittorrent","remotePath":"/downloads/","localPath":"/data/downloads/"}]`))
	}))
	defer srv.Close()
	got, err := NewClient(srv.URL, "k", 5*time.Second).RemotePathMappings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RemotePath != "/downloads/" || got[0].LocalPath != "/data/downloads/" {
		t.Errorf("mappings = %+v", got)
	}
}

func TestPostError_NamesNoFolderOrKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("stack trace with secrets"))
	}))
	defer srv.Close()
	_, err := NewClient(srv.URL, "super-secret", 5*time.Second).ManualImportCandidates(context.Background(), "/private/folder")
	if err == nil {
		t.Fatal("want error on 500")
	}
	for _, leak := range []string{"super-secret", "/private/folder", "stack trace"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
}

func TestTransportError_DropsURL(t *testing.T) {
	// A closed server: the transport error would normally embed the full URL,
	// query string (a folder path) included.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	_, err := NewClient(url, "k", time.Second).ManualImportCandidates(context.Background(), "/private/folder")
	if err == nil {
		t.Fatal("want a transport error")
	}
	if strings.Contains(err.Error(), "private") {
		t.Errorf("transport error leaks the folder: %v", err)
	}
}
