package domain

// Shared Topic.Extra keys written by the Sonarr poller. The infohash and source
// title pass Sonarr's grabbed variant identity to tracker plugins; the instance
// and series ids link the topic back to the Sonarr instance and series it came
// from, so a completed update can be imported into that series.
const (
	TopicExtraSonarrInfoHash    = "sonarr_infohash"
	TopicExtraSonarrSourceTitle = "sonarr_source_title"
	// TopicExtraSonarrInstanceID is the sonarr_instances.id (UUID string) of
	// the instance whose grab created or last refreshed the topic.
	TopicExtraSonarrInstanceID = "sonarr_instance_id"
	// TopicExtraSonarrSeriesID is that grab's Sonarr series id (a number;
	// JSON round-tripping makes it a float64, read it with extra.Int).
	TopicExtraSonarrSeriesID = "sonarr_series_id"
	// TopicExtraSonarrDownloadID is the grab's history downloadId — for a
	// torrent client, the infohash of the torrent Sonarr sent it. A second
	// "is this Sonarr's own grab?" signal for indexers whose feed carries no
	// torrentInfoHash.
	TopicExtraSonarrDownloadID = "sonarr_download_id"
	// TopicExtraSonarrGrabHashes is a bounded, newest-last list of every
	// grab hash (infohash and download id) Sonarr grabbed for the topic, so
	// an earlier grab is still recognised as Sonarr's own after a later one
	// overwrote the single-value keys above.
	TopicExtraSonarrGrabHashes = "sonarr_grab_hashes"
	// TopicExtraSonarrEndpoint is a short one-way fingerprint of the Sonarr
	// base URL the instance+series link was made against. The series id is
	// only valid on that server; the URL itself is not stored because topic
	// owners can read extra.
	TopicExtraSonarrEndpoint = "sonarr_endpoint_fp"
)
