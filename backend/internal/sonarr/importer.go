package sonarr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/artyomsv/marauder/backend/internal/crypto"
	"github.com/artyomsv/marauder/backend/internal/db/repo"
	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/events"
	"github.com/artyomsv/marauder/backend/internal/extra"
	"github.com/artyomsv/marauder/backend/internal/metrics"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	"github.com/artyomsv/marauder/backend/internal/progress"
)

// Import-on-complete results — the bounded label set of
// marauder_sonarr_imports_total{result}. One per completed delivery.
const (
	resultRequested       = "requested"         // ManualImport command queued
	resultNothingToImport = "nothing_to_import" // no file to import: nothing new, already in the library, or rejected
	resultNothingFound    = "nothing_found"     // Sonarr saw no video file of this torrent at the path
	resultNotSonarr       = "not_sonarr"        // topic not created by the Sonarr integration
	resultNotifyOnly      = "notify_only"       // topic is in notify-only (watch) mode
	resultOwnGrab         = "own_grab"          // a torrent Sonarr grabbed itself; Sonarr imports it
	resultNoLink          = "no_link"           // topic predates the instance/series link
	resultLinkStale       = "link_stale"        // link made against another Sonarr server, or changed meanwhile
	resultInstanceGone    = "instance_gone"     // linked instance was deleted
	resultInstanceOff     = "instance_disabled" // linked instance disabled or missing url/key
	resultOptionOff       = "option_off"        // instance has import-on-complete off
	resultTopicGone       = "topic_gone"        // topic deleted (or changed owner) before the import ran
	resultTorrentGone     = "torrent_gone"      // client no longer has the torrent
	resultNotReady        = "not_ready"         // client never reported the torrent complete and at rest
	resultNoFileList      = "no_file_list"      // client cannot list files / reported no save path
	resultPathAmbiguous   = "path_ambiguous"    // remote path mappings do not give one answer
	resultDropped         = "dropped"           // importer queue full
	resultError           = "error"             // a Marauder or Sonarr call failed
)

const (
	// importQueueSize bounds completions waiting for a worker. Completions are
	// rare (one per finished torrent); a full queue drops with a Warn rather
	// than ever blocking the progress watcher.
	importQueueSize = 64
	// importWorkers bounds concurrent imports, which is also the most Sonarr
	// folder scans Marauder runs at once.
	importWorkers = 2
	// importJobTimeout bounds one import end to end, settle wait included.
	importJobTimeout = 5 * time.Minute
	// importHTTPTimeout bounds each Sonarr call. A manual-import scan reads
	// media info for every video file, so it is far slower than a history read.
	importHTTPTimeout = 90 * time.Second
	// clientReadTimeout bounds each torrent-client re-read.
	clientReadTimeout = 15 * time.Second
	// settleDelay is waited before the first client re-read and between
	// re-reads. The watcher fires on 100%, but qBittorrent then moves the files
	// out of its temp folder ("moving", normalised to checking) and
	// Transmission out of its incomplete dir; reading paths before that ends
	// would point Sonarr at files that are about to disappear.
	settleDelay = 30 * time.Second
	// settleAttempts is how many re-reads may find the torrent unsettled.
	settleAttempts = 4
	// maxLoggedRejections caps the rejection reasons a log line carries.
	maxLoggedRejections = 10
)

// importInstances reads one Sonarr instance. Satisfied by *repo.SonarrInstances.
type importInstances interface {
	Get(ctx context.Context, master *crypto.MasterKey, id uuid.UUID) (*domain.SonarrInstance, error)
}

// importTopics reads one topic. Satisfied by *repo.Topics.
type importTopics interface {
	GetByID(ctx context.Context, id uuid.UUID, userID *uuid.UUID) (*domain.Topic, error)
}

// importEmitter records the outcome on the topic timeline. Satisfied by
// *events.Bus. Optional.
type importEmitter interface {
	Emit(ctx context.Context, ev events.Event)
}

// Importer asks Sonarr to import a torrent Marauder delivered for a
// Sonarr-created topic once it finishes downloading — the update Sonarr never
// grabbed and so never imports by itself. It implements
// progress.CompletionHook: the watcher hands over each won completion, the
// importer queues it and works it on its own goroutines, and nothing it does
// can fail or repeat the completion.
type Importer struct {
	log       zerolog.Logger
	master    *crypto.MasterKey
	instances importInstances
	topics    importTopics
	emit      importEmitter

	queue chan progress.Completion

	// Seams for tests.
	newClient func(baseURL, apiKey string) *Client
	sleep     func(ctx context.Context, d time.Duration) bool
	settle    time.Duration
}

// NewImporter builds an Importer. emit may be nil (no timeline entries).
func NewImporter(log zerolog.Logger, master *crypto.MasterKey, instances importInstances, topics importTopics, emit importEmitter) *Importer {
	return &Importer{
		log:       log.With().Str("component", "sonarr-importer").Logger(),
		master:    master,
		instances: instances,
		topics:    topics,
		emit:      emit,
		queue:     make(chan progress.Completion, importQueueSize),
		newClient: func(baseURL, apiKey string) *Client {
			return NewClient(baseURL, apiKey, importHTTPTimeout)
		},
		sleep:  sleepCtx,
		settle: settleDelay,
	}
}

// Start runs the workers until ctx is cancelled. It returns immediately.
// Completions queued before Start wait in the buffer.
func (i *Importer) Start(ctx context.Context) {
	for n := 0; n < importWorkers; n++ {
		go i.work(ctx)
	}
}

// DeliveryCompleted implements progress.CompletionHook. It never blocks: a
// full queue drops the completion with a Warn.
func (i *Importer) DeliveryCompleted(c progress.Completion) {
	select {
	case i.queue <- c:
	default:
		i.log.Warn().Str("topic_id", c.Delivery.TopicID.String()).Str("infohash", c.Delivery.Infohash).
			Msg("sonarr import queue full; completion not imported")
		metrics.SonarrImportsTotal.WithLabelValues(resultDropped).Inc()
	}
}

func (i *Importer) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-i.queue:
			i.handle(ctx, c)
		}
	}
}

// handle runs one import on its own budget and records the result. A panic is
// contained so one bad completion cannot stop a worker.
func (i *Importer) handle(ctx context.Context, c progress.Completion) {
	defer func() {
		if r := recover(); r != nil {
			i.log.Error().Interface("panic", r).Str("topic_id", c.Delivery.TopicID.String()).
				Msg("sonarr import panicked")
			metrics.SonarrImportsTotal.WithLabelValues(resultError).Inc()
		}
	}()
	jctx, cancel := context.WithTimeout(ctx, importJobTimeout)
	defer cancel()
	metrics.SonarrImportsTotal.WithLabelValues(i.importOne(jctx, c)).Inc()
}

// binding is what an import is allowed to act on: the topic, the instance it
// is linked to, and the link itself. It is resolved once at the start and
// again right before the command is queued, and the two must agree.
type binding struct {
	topic    *domain.Topic
	inst     *domain.SonarrInstance
	seriesID int
	endpoint string // the stored endpoint fingerprint, equal to inst's current one
}

func (b binding) sameLink(o binding) bool {
	return b.inst.ID == o.inst.ID && b.seriesID == o.seriesID && b.endpoint == o.endpoint
}

// importJob carries what the later steps log and record.
type importJob struct {
	d   domain.InFlightDelivery
	b   binding
	log zerolog.Logger
}

// importOne decides and, when everything lines up, requests the import. It
// returns the metric result.
func (i *Importer) importOne(ctx context.Context, c progress.Completion) string {
	d := c.Delivery
	log := i.log.With().Str("topic_id", d.TopicID.String()).Str("infohash", d.Infohash).Logger()

	b, result := i.bind(ctx, d, log)
	if result != "" {
		return result
	}
	log = log.With().Str("instance_id", b.inst.ID.String()).Str("instance", b.inst.Name).
		Int("series_id", b.seriesID).Logger()
	job := importJob{d: d, b: b, log: log}
	result, detail := i.run(ctx, c, job)
	i.record(ctx, job, result, detail)
	return result
}

// bind resolves and checks everything the import depends on that is not the
// torrent itself. A non-empty result means skip; each skip is logged here.
func (i *Importer) bind(ctx context.Context, d domain.InFlightDelivery, log zerolog.Logger) (binding, string) {
	// Scoped to the delivery's owner: a topic that changed hands reads as gone.
	topic, err := i.topics.GetByID(ctx, d.TopicID, &d.UserID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			log.Info().Msg("sonarr import skipped: topic no longer exists")
			return binding{}, resultTopicGone
		}
		log.Warn().Err(err).Msg("sonarr import skipped: topic lookup failed")
		return binding{}, resultError
	}
	if src, _ := topic.Extra["source"].(string); src != topicSourceSonarr {
		log.Debug().Msg("sonarr import skipped: topic was not created by the Sonarr integration")
		return binding{}, resultNotSonarr
	}
	if topic.NotifyOnly {
		log.Info().Msg("sonarr import skipped: topic is in notify-only mode")
		return binding{}, resultNotifyOnly
	}
	if isOwnGrab(topic.Extra, d.Infohash) {
		log.Info().Msg("sonarr import skipped: this is a torrent Sonarr grabbed, Sonarr imports it itself")
		return binding{}, resultOwnGrab
	}

	instRaw := extra.String(topic.Extra, domain.TopicExtraSonarrInstanceID, "")
	seriesID := extra.Int(topic.Extra, domain.TopicExtraSonarrSeriesID)
	instID, perr := uuid.Parse(instRaw)
	if instRaw == "" || perr != nil || seriesID <= 0 {
		log.Info().Msg("sonarr import skipped: topic predates the Sonarr instance/series link; " +
			"it is filled in the next time Sonarr grabs this release")
		return binding{}, resultNoLink
	}
	log = log.With().Str("instance_id", instID.String()).Int("series_id", seriesID).Logger()

	inst, err := i.instances.Get(ctx, i.master, instID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			log.Info().Msg("sonarr import skipped: the Sonarr instance that created this topic was deleted")
			return binding{}, resultInstanceGone
		}
		log.Warn().Err(err).Msg("sonarr import skipped: instance lookup failed")
		return binding{}, resultError
	}
	log = log.With().Str("instance", inst.Name).Logger()
	switch {
	case !inst.Enabled:
		log.Info().Msg("sonarr import skipped: instance is disabled")
		return binding{}, resultInstanceOff
	case inst.URL == "" || inst.APIKey == "":
		log.Info().Msg("sonarr import skipped: instance has no URL or API key")
		return binding{}, resultInstanceOff
	case !inst.ImportOnComplete:
		log.Info().Msg("sonarr import skipped: import on complete is off for this instance")
		return binding{}, resultOptionOff
	}
	// A series id is only valid on the server that issued it. A link without
	// a fingerprint predates the binding and cannot be trusted either.
	stored := extra.String(topic.Extra, domain.TopicExtraSonarrEndpoint, "")
	if stored == "" || stored != endpointFingerprint(inst.URL) {
		log.Info().Msg("sonarr import skipped: the topic's series link was made against a different " +
			"Sonarr URL than the instance has now; the next grab of this release re-links it")
		return binding{}, resultLinkStale
	}
	return binding{topic: topic, inst: inst, seriesID: seriesID, endpoint: stored}, ""
}

// run is the client + Sonarr half. detail is the human summary recorded on
// the timeline.
func (i *Importer) run(ctx context.Context, c progress.Completion, job importJob) (result, detail string) {
	log := job.log
	insp, result := i.settled(ctx, c, log)
	if result != "" {
		return result, "the torrent client did not report the finished files"
	}
	if !insp.FilesSupported || len(insp.Files) == 0 || strings.TrimSpace(insp.Status.SavePath) == "" {
		log.Warn().Bool("files_supported", insp.FilesSupported).Int("files", len(insp.Files)).
			Bool("save_path", insp.Status.SavePath != "").
			Msg("sonarr import skipped: the client did not report the torrent's files and save path")
		return resultNoFileList, "the torrent client did not report the torrent's files"
	}

	save := newOSPath(insp.Status.SavePath)
	all := make([]string, 0, len(insp.Files))
	var wanted []osPath
	for _, f := range insp.Files {
		all = append(all, f.Path)
		// A file the user (or only-new-files) skipped is absent or partial
		// on disk; it must never reach Sonarr.
		if f.Wanted {
			wanted = append(wanted, save.join(f.Path))
		}
	}
	if len(wanted) == 0 {
		log.Info().Msg("sonarr import skipped: every file of this torrent is skipped in the client")
		return resultNothingToImport, "every file of the torrent is skipped in the client"
	}

	b := job.b
	client := i.newClient(b.inst.URL, b.inst.APIKey)
	mappings, err := client.RemotePathMappings(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("sonarr import failed: reading remote path mappings")
		return resultError, "Sonarr could not be reached"
	}
	root := contentRoot(save, all)
	folder, mapped, err := mapPath(root, c.ClientHost, mappings)
	if err != nil {
		log.Warn().Str("client_path", root.native()).Str("client_host", c.ClientHost).
			Msg("sonarr import skipped: Sonarr's remote path mappings do not give one answer for this path " +
				"(no mapping names this client's host, and the matching ones disagree)")
		return resultPathAmbiguous, "Sonarr's remote path mappings are ambiguous for this path"
	}
	allowed := make(map[string]struct{}, len(wanted))
	for _, w := range wanted {
		m, _, merr := mapPath(w, c.ClientHost, mappings)
		if merr != nil {
			log.Warn().Str("client_path", w.native()).Str("client_host", c.ClientHost).
				Msg("sonarr import skipped: Sonarr's remote path mappings do not give one answer for a file of this torrent")
			return resultPathAmbiguous, "Sonarr's remote path mappings are ambiguous for this path"
		}
		allowed[m.key()] = struct{}{}
	}
	log = log.With().Str("path", folder.native()).Bool("remote_path_mapped", mapped).Logger()

	items, err := client.ManualImportCandidates(ctx, folder.native())
	if err != nil {
		log.Warn().Err(err).Msg("sonarr import failed: listing manual-import candidates")
		return resultError, "Sonarr could not list the files"
	}
	// Only this torrent's own wanted files. Without a root folder the path is
	// the whole save directory, and forcing our series onto a neighbour's
	// files would import the wrong show.
	var own []ManualImportItem
	for _, it := range items {
		if _, ok := allowed[newOSPath(it.Path).key()]; ok {
			own = append(own, it)
		}
	}
	if len(own) == 0 {
		log.Warn().Int("sonarr_files", len(items)).
			Msg("sonarr import skipped: Sonarr found none of this torrent's video files at the path; " +
				"if Sonarr sees the download directory under a different path, add a Remote Path Mapping in Sonarr")
		return resultNothingFound, "Sonarr found none of the torrent's video files at " + folder.native()
	}

	evaluated, err := client.ReprocessManualImport(ctx, b.seriesID, own)
	if err != nil {
		log.Warn().Err(err).Msg("sonarr import failed: evaluating the files against the series")
		return resultError, "Sonarr could not evaluate the files"
	}
	sel := selectImportable(b.seriesID, own, evaluated)
	if len(sel.files) == 0 {
		log.Info().Strs("rejections", sel.rejected).Int("candidates", len(own)).Int("already_in_library", sel.existing).
			Msg("sonarr import: nothing to import (every episode already has a file, or Sonarr rejected every file)")
		if sel.existing > 0 {
			return resultNothingToImport, "Nothing new: every episode in this torrent already has a file in Sonarr"
		}
		return resultNothingToImport, "Sonarr had nothing new to import from this torrent"
	}

	// The settle wait and the scan take a while; the topic or the instance may
	// have changed meanwhile. Files evaluated under one binding are never sent
	// under another.
	if res := i.revalidate(ctx, job); res != "" {
		return res, "the topic or its Sonarr instance changed while the import was being prepared"
	}

	cmd, err := client.ManualImport(ctx, sel.files)
	if err != nil {
		log.Warn().Err(err).Int("files", len(sel.files)).Msg("sonarr import failed: queueing the ManualImport command")
		return resultError, "Sonarr refused the import command"
	}
	log.Info().Int("files", len(sel.files)).Int("command_id", cmd.ID).Int("already_in_library", sel.existing).
		Strs("rejections", sel.rejected).Msg("sonarr import requested (copy mode)")
	detail = fmt.Sprintf("Asked Sonarr to import %d file(s)", len(sel.files))
	if sel.existing > 0 {
		detail += fmt.Sprintf("; skipped %d whose episodes already have a file in Sonarr", sel.existing)
	}
	return resultRequested, detail
}

// revalidate re-resolves the binding and requires it unchanged: same owner
// (the lookup is owner-scoped), same instance, series and endpoint, not
// notify-only, instance still enabled with the option on.
func (i *Importer) revalidate(ctx context.Context, job importJob) string {
	fresh, result := i.bind(ctx, job.d, job.log)
	if result != "" {
		return result
	}
	if !job.b.sameLink(fresh) {
		job.log.Info().Msg("sonarr import abandoned: the topic's Sonarr link changed while the import was being prepared")
		return resultLinkStale
	}
	return ""
}

// settled re-reads the torrent until the client reports it complete AND at
// rest — seeding, stopped or queued with every wanted byte present — so the
// save path and file list are final. A torrent that went incomplete after the
// completion (a file un-skipped, a recheck that found data missing) never
// passes. An empty result means ready.
func (i *Importer) settled(ctx context.Context, c progress.Completion, log zerolog.Logger) (progress.Inspection, string) {
	if c.Inspect == nil {
		return progress.Inspection{}, resultNoFileList
	}
	var last progress.Inspection
	var lastErr error
	for attempt := 0; attempt < settleAttempts; attempt++ {
		if !i.sleep(ctx, i.settle) {
			log.Warn().Msg("sonarr import abandoned: shutting down or out of time while waiting for the client")
			return last, resultNotReady
		}
		rctx, cancel := context.WithTimeout(ctx, clientReadTimeout)
		insp, err := c.Inspect(rctx)
		cancel()
		if err != nil {
			lastErr = err // sanitised by the watcher: no URL, no body
			continue
		}
		last, lastErr = insp, nil
		if !insp.Found {
			log.Info().Msg("sonarr import skipped: the torrent client no longer has this torrent")
			return insp, resultTorrentGone
		}
		if insp.Status.PercentDone < 1.0 {
			continue
		}
		switch insp.Status.State {
		case registry.StateSeeding, registry.StateStopped, registry.StateQueued:
			return insp, ""
		}
	}
	ev := log.Warn().Str("state", last.Status.State).Float64("percent_done", last.Status.PercentDone)
	if lastErr != nil {
		ev = ev.Err(lastErr)
	}
	ev.Msg("sonarr import skipped: the client never reported the torrent complete and at rest")
	return last, resultNotReady
}

// selection is selectImportable's answer.
type selection struct {
	files    []ManualImportFile
	rejected []string // why other files were left, capped for logging
	existing int      // files left because an episode already has a file
}

// selectImportable keeps the evaluated files Sonarr would import into our
// series as NEW episodes — resolved series, at least one episode, a quality,
// no rejection, and no episode that already has a file — and turns them into
// command files. Replacing an existing episode file is out of scope: Sonarr
// rejects only downgrades, so an equal-quality file passes its checks and the
// import would delete the library file before copying the new one. A file
// whose episode was already taken by an earlier one is dropped, as Sonarr's
// own UI refuses duplicate episodes in one import.
func selectImportable(seriesID int, own []ManualImportItem, evaluated []ReprocessedItem) selection {
	folderByPath := make(map[string]string, len(own))
	for _, it := range own {
		folderByPath[newOSPath(it.Path).key()] = it.FolderName
	}
	var sel selection
	reject := func(path, why string) {
		if len(sel.rejected) < maxLoggedRejections {
			sel.rejected = append(sel.rejected, baseName(path)+": "+why)
		}
	}
	seen := map[int]struct{}{}
	for _, ev := range evaluated {
		switch {
		case len(ev.Rejections) > 0:
			reasons := make([]string, 0, len(ev.Rejections))
			for _, r := range ev.Rejections {
				reasons = append(reasons, r.Reason)
			}
			reject(ev.Path, strings.Join(reasons, "; "))
			continue
		case ev.SeriesID != seriesID:
			reject(ev.Path, "not matched to the series")
			continue
		case len(ev.Episodes) == 0:
			reject(ev.Path, "no episode recognised")
			continue
		case !present(ev.Quality):
			reject(ev.Path, "no quality")
			continue
		}
		ids := make([]int, 0, len(ev.Episodes))
		dup, hasFile := false, false
		for _, e := range ev.Episodes {
			if _, ok := seen[e.ID]; ok {
				dup = true
			}
			if e.HasFile || e.EpisodeFileID > 0 {
				hasFile = true
			}
			ids = append(ids, e.ID)
		}
		if hasFile {
			sel.existing++
			reject(ev.Path, "episode already has a file in Sonarr")
			continue
		}
		if dup {
			reject(ev.Path, "episode already imported from another file of this torrent")
			continue
		}
		for _, id := range ids {
			seen[id] = struct{}{}
		}
		sel.files = append(sel.files, ManualImportFile{
			Path:         ev.Path,
			FolderName:   folderByPath[newOSPath(ev.Path).key()],
			SeriesID:     seriesID,
			EpisodeIDs:   ids,
			Quality:      ev.Quality,
			Languages:    rawOrNil(ev.Languages),
			Language:     rawOrNil(ev.Language),
			ReleaseGroup: ev.ReleaseGroup,
			IndexerFlags: ev.IndexerFlags,
			ReleaseType:  rawOrNil(ev.ReleaseType),
		})
	}
	return sel
}

// record writes the outcome to the topic timeline: requested at info, nothing
// to do at info, anything else at warn. Only reached once the instance has the
// option on, so a user never sees entries for an integration they did not ask
// for.
func (i *Importer) record(ctx context.Context, job importJob, result, detail string) {
	if i.emit == nil {
		return
	}
	sev := "warn"
	if result == resultRequested || result == resultNothingToImport {
		sev = "info"
	}
	tid := job.d.TopicID
	i.emit.Emit(ctx, events.Event{
		UserID: job.d.UserID, TopicID: &tid, NotifierID: job.b.topic.NotifierID,
		Type: events.SonarrImport, Severity: sev,
		Title: job.b.topic.DisplayName, Body: detail, SourceURL: job.b.topic.URL,
		Data: map[string]any{
			"result":   result,
			"infohash": job.d.Infohash,
			"instance": job.b.inst.Name,
		},
	})
}

// isOwnGrab reports whether infohash is a torrent Sonarr grabbed for this
// topic: any member of the topic's grab-hash set — the indexer's
// torrentInfoHash and the history downloadId (a torrent client's infohash,
// upper-cased by Sonarr) of each recent grab — or, for a topic written before
// the set existed, the single-value keys. Compared case-blind. With nothing
// recorded Marauder cannot tell and proceeds; the new-episodes-only selection
// then keeps it from replacing any file Sonarr already imported.
func isOwnGrab(topicExtra map[string]any, infohash string) bool {
	for _, grab := range storedGrabHashes(topicExtra) {
		if strings.EqualFold(grab, infohash) {
			return true
		}
	}
	return false
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

// sleepCtx waits d or until ctx ends; false means ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
