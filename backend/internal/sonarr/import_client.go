package sonarr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// The calls below drive Sonarr's interactive ("manual") import from code. Every
// shape was checked against Sonarr's source for v3 (tag 3.0.10.1567) and v4
// (tags v4.0.0.748 .. v4.0.20.3014 and develop): ManualImportController /
// ManualImportModule, ManualImportResource, ManualImportReprocessResource,
// ManualImportCommand, ManualImportFile, ImportMode, RemotePathMappingResource.
//
// Quality, language and release-type values are carried as raw JSON from
// Sonarr's own answer back to Sonarr, never modelled: v3 has a single
// `language`, v4 a `languages` list and a string `releaseType`, and Marauder
// has no business re-encoding either.

// RemotePathMapping mirrors RemotePathMappingResource (identical in v3/v4).
type RemotePathMapping struct {
	ID         int    `json:"id"`
	Host       string `json:"host"`
	RemotePath string `json:"remotePath"`
	LocalPath  string `json:"localPath"`
}

// ManualImportSeries is the part of a candidate's series Marauder reads.
type ManualImportSeries struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

// ManualImportEpisode is the part of a candidate's episode Marauder reads
// (EpisodeResource, v3 and v4). HasFile / EpisodeFileID say the episode is
// already in the library: import-on-complete imports new episodes only.
type ManualImportEpisode struct {
	ID            int  `json:"id"`
	HasFile       bool `json:"hasFile"`
	EpisodeFileID int  `json:"episodeFileId"`
}

// ImportRejection is why Sonarr refuses a candidate. v3 serialises
// `Rejection{reason,type}`, v4 `ImportRejectionResource{reason,type}`.
type ImportRejection struct {
	Reason string `json:"reason"`
}

// ManualImportItem is one GET /api/v3/manualimport candidate.
type ManualImportItem struct {
	Path         string                `json:"path"`
	FolderName   string                `json:"folderName"`
	Series       *ManualImportSeries   `json:"series"`
	SeasonNumber *int                  `json:"seasonNumber"`
	Episodes     []ManualImportEpisode `json:"episodes"`
	ReleaseGroup string                `json:"releaseGroup"`
	Quality      json.RawMessage       `json:"quality"`
	Languages    json.RawMessage       `json:"languages"` // v4
	Language     json.RawMessage       `json:"language"`  // v3
	IndexerFlags int                   `json:"indexerFlags"`
	ReleaseType  json.RawMessage       `json:"releaseType"` // v4
	Rejections   []ImportRejection     `json:"rejections"`
}

// ReprocessedItem is one POST /api/v3/manualimport answer: the request item
// with Sonarr's re-evaluation filled in. It is ManualImportReprocessResource,
// which carries seriesId rather than a series object.
type ReprocessedItem struct {
	Path         string                `json:"path"`
	SeriesID     int                   `json:"seriesId"`
	SeasonNumber *int                  `json:"seasonNumber"`
	Episodes     []ManualImportEpisode `json:"episodes"`
	ReleaseGroup string                `json:"releaseGroup"`
	Quality      json.RawMessage       `json:"quality"`
	Languages    json.RawMessage       `json:"languages"`
	Language     json.RawMessage       `json:"language"`
	IndexerFlags int                   `json:"indexerFlags"`
	ReleaseType  json.RawMessage       `json:"releaseType"`
	Rejections   []ImportRejection     `json:"rejections"`
}

// reprocessRequest is one ManualImportReprocessResource sent to Sonarr.
//
// EpisodeIDs is an empty list and SeasonNumber is omitted on purpose: with
// neither, ManualImportService.ReprocessItem (v3 and v4) falls through to
// ProcessFile(…, series) — it parses the episodes from the file name against
// the series we name and runs the full import decision, rejections included.
//
// Languages is always an empty list, never the scanned one: v4's
// ManualImportController.ReprocessItems dereferences it unguarded (a missing
// list answers 500), and v4.0.0.748 calls SingleOrDefault on it with no count
// guard, so a candidate with two languages sent back answers 500 too. An empty
// list makes Sonarr fill in the languages it parses; the command then uses the
// languages from the reprocess RESPONSE. v3's singular `language` is passed
// through unchanged (its controller compares it, it does not enumerate it).
type reprocessRequest struct {
	Path         string          `json:"path"`
	SeriesID     int             `json:"seriesId"`
	EpisodeIDs   []int           `json:"episodeIds"`
	Quality      json.RawMessage `json:"quality,omitempty"`
	Languages    json.RawMessage `json:"languages"`
	Language     json.RawMessage `json:"language,omitempty"`
	ReleaseGroup string          `json:"releaseGroup,omitempty"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  json.RawMessage `json:"releaseType,omitempty"`
}

// ManualImportFile is one file of the ManualImport command (ManualImportFile in
// Sonarr). DownloadId is deliberately never sent: the new torrent is not a
// download Sonarr tracks, and naming one would make Sonarr treat the import as
// finishing that download.
type ManualImportFile struct {
	Path         string          `json:"path"`
	FolderName   string          `json:"folderName,omitempty"`
	SeriesID     int             `json:"seriesId"`
	EpisodeIDs   []int           `json:"episodeIds"`
	Quality      json.RawMessage `json:"quality"`
	Languages    json.RawMessage `json:"languages,omitempty"`
	Language     json.RawMessage `json:"language,omitempty"`
	ReleaseGroup string          `json:"releaseGroup,omitempty"`
	IndexerFlags int             `json:"indexerFlags"`
	ReleaseType  json.RawMessage `json:"releaseType,omitempty"`
}

// importModeCopy is ImportMode.Copy (camel-cased string, as Sonarr's own UI
// sends it). Never "move" — the torrent must keep seeding — and never "auto":
// for a download Sonarr does not track, auto resolves to a move.
const importModeCopy = "copy"

// manualImportCommand is the POST /api/v3/command body.
type manualImportCommand struct {
	Name       string             `json:"name"`
	Files      []ManualImportFile `json:"files"`
	ImportMode string             `json:"importMode"`
}

// CommandResult is the part of Sonarr's CommandResource Marauder logs.
type CommandResult struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// RemotePathMappings lists Sonarr's remote path mappings.
func (c *Client) RemotePathMappings(ctx context.Context) ([]RemotePathMapping, error) {
	var out []RemotePathMapping
	if err := c.getJSON(ctx, "/api/v3/remotepathmapping", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ManualImportCandidates asks Sonarr which video files under folder it could
// import. folder may be a directory or a single file (both v3 and v4 handle a
// file path by processing just that file).
//
// seriesId is deliberately NOT sent. In every released v3 and v4 build,
// ManualImportController.GetMediaFiles answers a request carrying seriesId
// with the series' EXISTING library files and ignores folder entirely (only
// v4's develop branch narrowed that to "seriesId without downloadId"). The
// series is applied afterwards through ReprocessManualImport instead.
func (c *Client) ManualImportCandidates(ctx context.Context, folder string) ([]ManualImportItem, error) {
	q := url.Values{}
	q.Set("folder", folder)
	q.Set("filterExistingFiles", "true")
	var out []ManualImportItem
	if err := c.getJSON(ctx, "/api/v3/manualimport", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReprocessManualImport re-evaluates candidates against seriesID — the
// interactive-import "select series" step. It changes nothing in Sonarr.
func (c *Client) ReprocessManualImport(ctx context.Context, seriesID int, items []ManualImportItem) ([]ReprocessedItem, error) {
	reqs := make([]reprocessRequest, 0, len(items))
	for _, it := range items {
		reqs = append(reqs, reprocessRequest{
			Path:         it.Path,
			SeriesID:     seriesID,
			EpisodeIDs:   []int{},
			Quality:      rawOrNil(it.Quality),
			Languages:    json.RawMessage("[]"),
			Language:     rawOrNil(it.Language),
			ReleaseGroup: it.ReleaseGroup,
			IndexerFlags: it.IndexerFlags,
			ReleaseType:  rawOrNil(it.ReleaseType),
		})
	}
	var out []ReprocessedItem
	if err := c.postJSON(ctx, "/api/v3/manualimport", reqs, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ManualImport queues Sonarr's ManualImport command in copy mode.
func (c *Client) ManualImport(ctx context.Context, files []ManualImportFile) (*CommandResult, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("sonarr: manual import: no files")
	}
	var out CommandResult
	body := manualImportCommand{Name: "ManualImport", Files: files, ImportMode: importModeCopy}
	if err := c.postJSON(ctx, "/api/v3/command", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// present reports whether a raw JSON value is set and not null.
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// rawOrNil drops an explicit JSON null so omitempty can omit the field.
func rawOrNil(raw json.RawMessage) json.RawMessage {
	if !present(raw) {
		return nil
	}
	return raw
}
