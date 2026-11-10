package sonarr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/artyomsv/marauder/backend/internal/crypto"
	"github.com/artyomsv/marauder/backend/internal/db/repo"
	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/events"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
	"github.com/artyomsv/marauder/backend/internal/progress"
)

// --- fakes ---------------------------------------------------------------

// fakeImportTopics answers successive GetByID calls from topics in order and
// repeats the last one, so a test can change the topic between the first
// binding and the pre-command revalidation.
type fakeImportTopics struct {
	topics []*domain.Topic
	err    error
	calls  int
}

func (f *fakeImportTopics) GetByID(context.Context, uuid.UUID, *uuid.UUID) (*domain.Topic, error) {
	if f.err != nil {
		return nil, f.err
	}
	t := f.topics[min(f.calls, len(f.topics)-1)]
	f.calls++
	return t, nil
}

func topicsOf(ts ...*domain.Topic) *fakeImportTopics { return &fakeImportTopics{topics: ts} }

// fakeImportInstances works like fakeImportTopics for instances.
type fakeImportInstances struct {
	insts []*domain.SonarrInstance
	err   error
	calls int
}

func (f *fakeImportInstances) Get(context.Context, *crypto.MasterKey, uuid.UUID) (*domain.SonarrInstance, error) {
	if f.err != nil {
		return nil, f.err
	}
	inst := f.insts[min(f.calls, len(f.insts)-1)]
	f.calls++
	return inst, nil
}

func instancesOf(is ...*domain.SonarrInstance) *fakeImportInstances {
	return &fakeImportInstances{insts: is}
}

type fakeImportEmitter struct{ events []events.Event }

func (f *fakeImportEmitter) Emit(_ context.Context, ev events.Event) { f.events = append(f.events, ev) }

// fakeSonarr serves the four import endpoints and records what it was sent.
type fakeSonarr struct {
	mu         sync.Mutex
	mappings   string
	candidates string
	reprocess  string
	folder     string
	reprocBody []map[string]json.RawMessage
	command    map[string]json.RawMessage
	commands   int
}

func (f *fakeSonarr) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v3/remotepathmapping":
			_, _ = w.Write([]byte(orDefault(f.mappings, "[]")))
		case r.URL.Path == "/api/v3/manualimport" && r.Method == http.MethodGet:
			f.folder = r.URL.Query().Get("folder")
			_, _ = w.Write([]byte(orDefault(f.candidates, "[]")))
		case r.URL.Path == "/api/v3/manualimport" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &f.reprocBody)
			_, _ = w.Write([]byte(orDefault(f.reprocess, "[]")))
		case r.URL.Path == "/api/v3/command":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &f.command)
			f.commands++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":5,"name":"ManualImport","status":"queued"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// --- fixtures ------------------------------------------------------------

const grabHash = "aaaa"
const updateHash = "bbbb"

// linkedTopic is a Sonarr-created topic linked to inst's current server.
func linkedTopic(inst *domain.SonarrInstance) *domain.Topic {
	return &domain.Topic{
		ID: uuid.New(), DisplayName: "Show", URL: "https://tracker.test/t=1",
		Extra: map[string]any{
			"source":                          topicSourceSonarr,
			domain.TopicExtraSonarrInfoHash:   "AAAA", // case differs on purpose
			domain.TopicExtraSonarrInstanceID: inst.ID.String(),
			domain.TopicExtraSonarrSeriesID:   float64(42), // as read back from JSONB
			domain.TopicExtraSonarrEndpoint:   endpointFingerprint(inst.URL),
		},
	}
}

func enabledInstance(url string) *domain.SonarrInstance {
	return &domain.SonarrInstance{ID: uuid.New(), Name: "TV", Enabled: true, URL: url, APIKey: "k", ImportOnComplete: true}
}

func completion(hash string, insp progress.Inspection, inspErr error) progress.Completion {
	return progress.Completion{
		Delivery: domain.InFlightDelivery{DeliveryID: uuid.New(), TopicID: uuid.New(), UserID: uuid.New(), Infohash: hash},
		Inspect: func(context.Context) (progress.Inspection, error) {
			return insp, inspErr
		},
	}
}

func seedingInspection(files ...domain.ClientFile) progress.Inspection {
	return progress.Inspection{
		Status:         registry.TorrentStatus{Hash: updateHash, PercentDone: 1, State: registry.StateSeeding, SavePath: "/downloads/tv"},
		Found:          true,
		FilesSupported: true,
		Files:          files,
	}
}

func newTestImporter(topics importTopics, instances importInstances, emit importEmitter) *Importer {
	imp := NewImporter(zerolog.Nop(), nil, instances, topics, emit)
	imp.sleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	imp.newClient = func(baseURL, apiKey string) *Client { return NewClient(baseURL, apiKey, 5*time.Second) }
	return imp
}

// candidate is one GET manualimport item for path, series unresolved.
func candidate(path, langs string) string {
	return `{"path":"` + path + `","folderName":"Show S01","series":null,"episodes":[],` +
		`"quality":{"quality":{"id":3}},"languages":` + langs + `,"rejections":[]}`
}

// evaluated is one reprocess answer for path with one episode. hasFile is what
// Sonarr reports for an episode already in the library — an equal-quality
// file is NOT rejected, only its episode says it has a file.
func evaluated(path string, episodeID int, hasFile bool, rejections string) string {
	fileID, hf := "0", "false"
	if hasFile {
		fileID, hf = "900", "true"
	}
	return `{"path":"` + path + `","seriesId":42,"episodes":[{"id":` + strconv.Itoa(episodeID) +
		`,"hasFile":` + hf + `,"episodeFileId":` + fileID + `}],"quality":{"quality":{"id":3}},` +
		`"languages":[{"id":1},{"id":11}],"rejections":` + rejections + `}`
}

func wantedFiles(paths ...string) []domain.ClientFile {
	out := make([]domain.ClientFile, 0, len(paths))
	for i, p := range paths {
		out = append(out, domain.ClientFile{Index: i, Path: p, Wanted: true})
	}
	return out
}

// --- tests ---------------------------------------------------------------

func TestImporter_RequestsCopyImportOfTheTorrentsOwnNewFiles(t *testing.T) {
	const dir = "/data/downloads/tv/Show S01/"
	fs := &fakeSonarr{
		mappings: `[{"id":1,"host":"qbit","remotePath":"/downloads/","localPath":"/data/downloads/"}]`,
		candidates: "[" + candidate(dir+"E05.mkv", `[{"id":1},{"id":11}]`) + "," +
			candidate(dir+"E01.mkv", `[{"id":1}]`) + "," +
			candidate(dir+"E06.mkv", `[{"id":1}]`) + "]",
		reprocess: "[" + evaluated(dir+"E05.mkv", 105, false, `[]`) + "," +
			evaluated(dir+"E01.mkv", 101, true, `[]`) + "]",
	}
	srv := fs.server(t)
	inst := enabledInstance(srv.URL)
	emit := &fakeImportEmitter{}
	imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), emit)

	insp := seedingInspection(wantedFiles("Show S01/E01.mkv", "Show S01/E05.mkv")...)
	// Skipped by only-new-files: on disk partially at best, never imported.
	insp.Files = append(insp.Files, domain.ClientFile{Index: 2, Path: "Show S01/E06.mkv", Wanted: false})
	c := completion(updateHash, insp, nil)
	c.ClientHost = "qbit"

	if got := imp.importOne(context.Background(), c); got != resultRequested {
		t.Fatalf("result = %q, want %q", got, resultRequested)
	}
	if fs.folder != "/data/downloads/tv/Show S01" {
		t.Errorf("folder = %q, want the mapped torrent root", fs.folder)
	}
	if len(fs.reprocBody) != 2 {
		t.Fatalf("reprocess sent %d items, want 2 (the unwanted E06 must be filtered out)", len(fs.reprocBody))
	}
	for _, it := range fs.reprocBody {
		if string(it["seriesId"]) != "42" {
			t.Errorf("reprocess seriesId = %s", it["seriesId"])
		}
		// v4.0.0.748 answers 500 to a two-language list; never send the scan's.
		if string(it["languages"]) != "[]" {
			t.Errorf("reprocess languages = %s, want []", it["languages"])
		}
	}
	if fs.commands != 1 || string(fs.command["importMode"]) != `"copy"` {
		t.Fatalf("command = %v (count %d)", fs.command, fs.commands)
	}
	var files []map[string]json.RawMessage
	_ = json.Unmarshal(fs.command["files"], &files)
	if len(files) != 1 || string(files[0]["path"]) != `"`+dir+`E05.mkv"` || string(files[0]["episodeIds"]) != "[105]" {
		t.Fatalf("command files = %s; E01 already has a file and must not be replaced", fs.command["files"])
	}
	if string(files[0]["folderName"]) != `"Show S01"` {
		t.Errorf("folderName not carried from the scan: %s", files[0]["folderName"])
	}
	if string(files[0]["languages"]) != `[{"id":1},{"id":11}]` {
		t.Errorf("command languages must come from the reprocess response, got %s", files[0]["languages"])
	}
	if len(emit.events) != 1 || emit.events[0].Type != events.SonarrImport || emit.events[0].Severity != "info" {
		t.Errorf("timeline events = %+v", emit.events)
	}
}

func TestImporter_UpdatedPackOnlyImportsTheNewEpisode(t *testing.T) {
	// The library has E01–E04; the updated pack carries E01–E05 at the same
	// quality. Sonarr would accept all five and replace E01–E04 (it rejects
	// downgrades, not equal quality); only E05 may be sent.
	const dir = "/downloads/tv/Show/"
	var cands, evals, paths []string
	for n := 1; n <= 5; n++ {
		name := "E0" + strconv.Itoa(n) + ".mkv"
		cands = append(cands, candidate(dir+name, `[{"id":1}]`))
		evals = append(evals, evaluated(dir+name, 100+n, n <= 4, `[]`))
		paths = append(paths, "Show/"+name)
	}
	fs := &fakeSonarr{candidates: "[" + strings.Join(cands, ",") + "]", reprocess: "[" + strings.Join(evals, ",") + "]"}
	srv := fs.server(t)
	inst := enabledInstance(srv.URL)
	emit := &fakeImportEmitter{}
	imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), emit)

	c := completion(updateHash, seedingInspection(wantedFiles(paths...)...), nil)
	if got := imp.importOne(context.Background(), c); got != resultRequested {
		t.Fatalf("result = %q", got)
	}
	var files []map[string]json.RawMessage
	_ = json.Unmarshal(fs.command["files"], &files)
	if len(files) != 1 || string(files[0]["episodeIds"]) != "[105]" {
		t.Fatalf("command files = %s, want only E05", fs.command["files"])
	}
	want := "Asked Sonarr to import 1 file(s); skipped 4 whose episodes already have a file in Sonarr"
	if len(emit.events) != 1 || emit.events[0].Body != want {
		t.Errorf("timeline events = %+v", emit.events)
	}
}

func TestImporter_OwnGrabSkipped(t *testing.T) {
	inst := enabledInstance("http://sonarr.invalid")
	emit := &fakeImportEmitter{}
	imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), emit)
	if got := imp.importOne(context.Background(), completion(grabHash, seedingInspection(), nil)); got != resultOwnGrab {
		t.Fatalf("result = %q, want %q", got, resultOwnGrab)
	}
	if len(emit.events) != 0 {
		t.Error("a skip before the option check must not touch the timeline")
	}
}

func TestImporter_EarlierGrabStillOwnAfterAReGrab(t *testing.T) {
	// Sonarr grabbed A, then B for the same topic before A completed: the
	// single-value key now says B, but A is still Sonarr's own download.
	inst := enabledInstance("http://sonarr.invalid")
	topic := linkedTopic(inst)
	topic.Extra[domain.TopicExtraSonarrInfoHash] = "BBBB-GRAB"
	topic.Extra[domain.TopicExtraSonarrGrabHashes] = []any{"aaaa", "bbbb-grab"}
	imp := newTestImporter(topicsOf(topic), instancesOf(inst), nil)
	if got := imp.importOne(context.Background(), completion("AAAA", seedingInspection(), nil)); got != resultOwnGrab {
		t.Fatalf("result = %q, want %q", got, resultOwnGrab)
	}
}

func TestImporter_Skips(t *testing.T) {
	inst := enabledInstance("http://sonarr.invalid")
	off := enabledInstance(inst.URL)
	off.ID, off.ImportOnComplete = inst.ID, false
	disabled := enabledInstance(inst.URL)
	disabled.ID, disabled.Enabled = inst.ID, false
	repointed := enabledInstance("http://other-sonarr.invalid:8989")
	repointed.ID = inst.ID

	manual := linkedTopic(inst)
	manual.Extra = map[string]any{"provider": "x"}
	legacy := linkedTopic(inst)
	delete(legacy.Extra, domain.TopicExtraSonarrInstanceID)
	delete(legacy.Extra, domain.TopicExtraSonarrSeriesID)
	unbound := linkedTopic(inst) // a pair without the endpoint fingerprint
	delete(unbound.Extra, domain.TopicExtraSonarrEndpoint)
	notifyOnly := linkedTopic(inst)
	notifyOnly.NotifyOnly = true

	tests := []struct {
		name      string
		topics    *fakeImportTopics
		instances *fakeImportInstances
		want      string
	}{
		{"topic deleted", &fakeImportTopics{err: repo.ErrNotFound}, instancesOf(inst), resultTopicGone},
		{"topic read fails", &fakeImportTopics{err: errors.New("db down")}, instancesOf(inst), resultError},
		{"manual topic", topicsOf(manual), instancesOf(inst), resultNotSonarr},
		{"notify-only topic", topicsOf(notifyOnly), instancesOf(inst), resultNotifyOnly},
		{"legacy topic without link", topicsOf(legacy), instancesOf(inst), resultNoLink},
		{"link without endpoint fingerprint", topicsOf(unbound), instancesOf(inst), resultLinkStale},
		{"instance re-pointed at another server", topicsOf(linkedTopic(inst)), instancesOf(repointed), resultLinkStale},
		{"instance deleted", topicsOf(linkedTopic(inst)), &fakeImportInstances{err: repo.ErrNotFound}, resultInstanceGone},
		{"instance disabled", topicsOf(linkedTopic(inst)), instancesOf(disabled), resultInstanceOff},
		{"option off", topicsOf(linkedTopic(inst)), instancesOf(off), resultOptionOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emit := &fakeImportEmitter{}
			imp := newTestImporter(tt.topics, tt.instances, emit)
			if got := imp.importOne(context.Background(), completion(updateHash, seedingInspection(), nil)); got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
			if len(emit.events) != 0 {
				t.Errorf("skip %q must not write a timeline entry", tt.want)
			}
		})
	}
}

func TestImporter_APIKeyRotationKeepsTheLink(t *testing.T) {
	inst := enabledInstance("http://sonarr.invalid:8989/")
	topic := linkedTopic(inst)
	rotated := *inst
	rotated.APIKey = "new-key"
	rotated.URL = "HTTP://Sonarr.Invalid:8989" // cosmetic edit, same server
	imp := newTestImporter(topicsOf(topic), instancesOf(&rotated), nil)
	b, result := imp.bind(context.Background(), domain.InFlightDelivery{Infohash: updateHash}, zerolog.Nop())
	if result != "" || b.inst == nil {
		t.Fatalf("result = %q; a key rotation or cosmetic URL edit must not invalidate the link", result)
	}
}

// bindingChange derives the topic/instance the revalidation reads.
type bindingChange func(inst *domain.SonarrInstance, topic *domain.Topic) (*domain.SonarrInstance, *domain.Topic)

func TestImporter_RevalidatesBeforeTheCommand(t *testing.T) {
	const p = "/downloads/tv/Show/E05.mkv"
	tests := []struct {
		name   string
		change bindingChange
		want   string
	}{
		{"switched to notify-only while downloading", func(i *domain.SonarrInstance, tp *domain.Topic) (*domain.SonarrInstance, *domain.Topic) {
			n := *tp
			n.NotifyOnly = true
			return i, &n
		}, resultNotifyOnly},
		{"option turned off", func(i *domain.SonarrInstance, tp *domain.Topic) (*domain.SonarrInstance, *domain.Topic) {
			n := *i
			n.ImportOnComplete = false
			return &n, tp
		}, resultOptionOff},
		{"instance disabled", func(i *domain.SonarrInstance, tp *domain.Topic) (*domain.SonarrInstance, *domain.Topic) {
			n := *i
			n.Enabled = false
			return &n, tp
		}, resultInstanceOff},
		{"instance re-pointed", func(i *domain.SonarrInstance, tp *domain.Topic) (*domain.SonarrInstance, *domain.Topic) {
			n := *i
			n.URL = "http://elsewhere.invalid"
			return &n, tp
		}, resultLinkStale},
		{"re-linked to another series", func(i *domain.SonarrInstance, tp *domain.Topic) (*domain.SonarrInstance, *domain.Topic) {
			n := *tp
			n.Extra = map[string]any{}
			for k, v := range tp.Extra {
				n.Extra[k] = v
			}
			n.Extra[domain.TopicExtraSonarrSeriesID] = float64(43)
			return i, &n
		}, resultLinkStale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &fakeSonarr{
				candidates: "[" + candidate(p, `[{"id":1}]`) + "]",
				reprocess:  "[" + evaluated(p, 105, false, `[]`) + "]",
			}
			srv := fs.server(t)
			inst := enabledInstance(srv.URL)
			topic := linkedTopic(inst)
			inst2, topic2 := tt.change(inst, topic)
			imp := newTestImporter(topicsOf(topic, topic2), instancesOf(inst, inst2), nil)
			c := completion(updateHash, seedingInspection(wantedFiles("Show/E05.mkv")...), nil)
			if got := imp.importOne(context.Background(), c); got != tt.want {
				t.Fatalf("result = %q, want %q", got, tt.want)
			}
			if fs.commands != 0 {
				t.Error("a binding that changed after evaluation must not reach the command")
			}
		})
	}
}

func TestImporter_ClientSideOutcomes(t *testing.T) {
	gone := seedingInspection()
	gone.Found = false
	moving := seedingInspection(wantedFiles("Show/E01.mkv")...)
	moving.Status.State = registry.StateChecking
	incomplete := seedingInspection(wantedFiles("Show/E01.mkv")...)
	incomplete.Status.State = registry.StateStopped // at rest, but a recheck found data missing
	incomplete.Status.PercentDone = 0.93
	noFiles := seedingInspection()
	noFiles.FilesSupported = false
	noSave := seedingInspection(wantedFiles("Show/E01.mkv")...)
	noSave.Status.SavePath = ""
	allSkipped := seedingInspection(domain.ClientFile{Path: "Show/E01.mkv", Wanted: false})

	tests := []struct {
		name    string
		insp    progress.Inspection
		inspErr error
		want    string
	}{
		{"torrent removed from client", gone, nil, resultTorrentGone},
		{"still moving after every re-read", moving, nil, resultNotReady},
		{"stopped but incomplete", incomplete, nil, resultNotReady},
		{"client unreachable on every re-read", progress.Inspection{}, errors.New("status: torrent client unreachable"), resultNotReady},
		{"client cannot list files", noFiles, nil, resultNoFileList},
		{"no save path", noSave, nil, resultNoFileList},
		{"every file skipped", allSkipped, nil, resultNothingToImport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &fakeSonarr{}
			srv := fs.server(t)
			inst := enabledInstance(srv.URL)
			emit := &fakeImportEmitter{}
			imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), emit)
			if got := imp.importOne(context.Background(), completion(updateHash, tt.insp, tt.inspErr)); got != tt.want {
				t.Errorf("result = %q, want %q", got, tt.want)
			}
			if fs.commands != 0 {
				t.Error("no command may be sent")
			}
			if len(emit.events) != 1 {
				t.Errorf("an attempted import records exactly one timeline entry, got %d", len(emit.events))
			}
		})
	}
}

func TestImporter_NoRootFolder_NeverTouchesNeighbours(t *testing.T) {
	// A multi-file torrent without a root folder: the scan path is the whole
	// save directory, which also holds another show. Only the torrent's own
	// file may be evaluated against our series.
	fs := &fakeSonarr{
		candidates: `[` +
			`{"path":"/downloads/tv/Other.Show.S01E01.mkv","series":{"id":9},"episodes":[{"id":900}],"quality":{"quality":{"id":3}},"rejections":[]},` +
			candidate("/downloads/tv/E02.mkv", `[]`) +
			`]`,
		reprocess: "[" + evaluated("/downloads/tv/E02.mkv", 102, false, `[]`) + "]",
	}
	srv := fs.server(t)
	inst := enabledInstance(srv.URL)
	imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), nil)
	c := completion(updateHash, seedingInspection(wantedFiles("E02.mkv", "E02.nfo")...), nil)
	if got := imp.importOne(context.Background(), c); got != resultRequested {
		t.Fatalf("result = %q", got)
	}
	if fs.folder != "/downloads/tv" {
		t.Errorf("folder = %q", fs.folder)
	}
	if len(fs.reprocBody) != 1 || string(fs.reprocBody[0]["path"]) != `"/downloads/tv/E02.mkv"` {
		t.Errorf("reprocess body = %v; a neighbour's file must never be forced onto our series", fs.reprocBody)
	}
}

func TestImporter_NothingFoundAndNothingToImport(t *testing.T) {
	const p = "/downloads/tv/Show/E01.mkv"
	t.Run("sonarr sees no file at the path", func(t *testing.T) {
		fs := &fakeSonarr{candidates: `[]`}
		srv := fs.server(t)
		inst := enabledInstance(srv.URL)
		emit := &fakeImportEmitter{}
		imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), emit)
		c := completion(updateHash, seedingInspection(wantedFiles("Show/E01.mkv")...), nil)
		if got := imp.importOne(context.Background(), c); got != resultNothingFound {
			t.Fatalf("result = %q", got)
		}
		if len(emit.events) != 1 || emit.events[0].Severity != "warn" {
			t.Errorf("events = %+v", emit.events)
		}
	})
	t.Run("every episode already in the library", func(t *testing.T) {
		fs := &fakeSonarr{candidates: "[" + candidate(p, `[]`) + "]", reprocess: "[" + evaluated(p, 1, true, `[]`) + "]"}
		srv := fs.server(t)
		inst := enabledInstance(srv.URL)
		emit := &fakeImportEmitter{}
		imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), emit)
		c := completion(updateHash, seedingInspection(wantedFiles("Show/E01.mkv")...), nil)
		if got := imp.importOne(context.Background(), c); got != resultNothingToImport {
			t.Fatalf("result = %q", got)
		}
		if fs.commands != 0 {
			t.Error("nothing new must not send a command")
		}
		want := "Nothing new: every episode in this torrent already has a file in Sonarr"
		if len(emit.events) != 1 || emit.events[0].Severity != "info" || emit.events[0].Body != want {
			t.Errorf("events = %+v", emit.events)
		}
	})
	t.Run("rejected by Sonarr", func(t *testing.T) {
		fs := &fakeSonarr{
			candidates: "[" + candidate(p, `[]`) + "]",
			reprocess:  "[" + evaluated(p, 1, false, `[{"reason":"Sample"}]`) + "]",
		}
		srv := fs.server(t)
		inst := enabledInstance(srv.URL)
		imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), nil)
		c := completion(updateHash, seedingInspection(wantedFiles("Show/E01.mkv")...), nil)
		if got := imp.importOne(context.Background(), c); got != resultNothingToImport || fs.commands != 0 {
			t.Fatalf("result = %q, commands = %d", got, fs.commands)
		}
	})
}

// abMappings: client A maps /downloads, client B maps the deeper /downloads/tv.
const abMappings = `[` +
	`{"host":"qbit-a","remotePath":"/downloads/","localPath":"/mnt/a/"},` +
	`{"host":"qbit-b","remotePath":"/downloads/tv/","localPath":"/mnt/b/"}]`

func TestImporter_PathMappingForAnotherClientRefused(t *testing.T) {
	// A completes, and Sonarr knows A under a name Marauder does not use: the
	// two mappings disagree, so nothing may be scanned.
	fs := &fakeSonarr{mappings: abMappings}
	srv := fs.server(t)
	inst := enabledInstance(srv.URL)
	imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), nil)
	c := completion(updateHash, seedingInspection(wantedFiles("Show/E01.mkv")...), nil)
	c.ClientHost = "transmission"
	if got := imp.importOne(context.Background(), c); got != resultPathAmbiguous {
		t.Fatalf("result = %q", got)
	}
	if fs.folder != "" {
		t.Error("an ambiguous mapping must stop before any scan")
	}
}

func TestImporter_PathMappingUsesTheClientsHost(t *testing.T) {
	fs := &fakeSonarr{mappings: abMappings}
	srv := fs.server(t)
	inst := enabledInstance(srv.URL)
	imp := newTestImporter(topicsOf(linkedTopic(inst)), instancesOf(inst), nil)
	c := completion(updateHash, seedingInspection(wantedFiles("Show/E01.mkv")...), nil)
	c.ClientHost = "QBIT-A"
	_ = imp.importOne(context.Background(), c)
	if fs.folder != "/mnt/a/tv/Show" {
		t.Errorf("folder = %q, want client A's mapping applied, never B's deeper one", fs.folder)
	}
}

func TestSelectImportable(t *testing.T) {
	own := []ManualImportItem{
		{Path: "/a/E01.mkv", FolderName: "a"}, {Path: "/a/E01.v2.mkv"}, {Path: "/a/E02.mkv"},
		{Path: "/a/x.mkv"}, {Path: "/a/y.mkv"}, {Path: "/a/E03.mkv"},
	}
	q := json.RawMessage(`{"quality":{"id":3}}`)
	evaluated := []ReprocessedItem{
		{Path: "/a/E01.mkv", SeriesID: 42, Episodes: []ManualImportEpisode{{ID: 1}}, Quality: q},
		// duplicate episode
		{Path: "/a/E01.v2.mkv", SeriesID: 42, Episodes: []ManualImportEpisode{{ID: 1}}, Quality: q},
		// no quality
		{Path: "/a/E02.mkv", SeriesID: 42, Episodes: []ManualImportEpisode{{ID: 2}}},
		// other series
		{Path: "/a/x.mkv", SeriesID: 7, Episodes: []ManualImportEpisode{{ID: 3}}, Quality: q},
		// no episode
		{Path: "/a/y.mkv", SeriesID: 42, Quality: q},
		// a double episode, one half already in the library
		{Path: "/a/E03.mkv", SeriesID: 42, Episodes: []ManualImportEpisode{{ID: 4}, {ID: 5, EpisodeFileID: 77}}, Quality: q},
	}
	sel := selectImportable(42, own, evaluated)
	if len(sel.files) != 1 || sel.files[0].Path != "/a/E01.mkv" || sel.files[0].FolderName != "a" {
		t.Fatalf("files = %+v", sel.files)
	}
	if len(sel.rejected) != 5 || sel.existing != 1 {
		t.Errorf("rejected = %v (want 5), existing = %d (want 1)", sel.rejected, sel.existing)
	}
}

func TestImporter_DeliveryCompletedNeverBlocks(t *testing.T) {
	imp := NewImporter(zerolog.Nop(), nil, &fakeImportInstances{}, &fakeImportTopics{}, nil)
	// No workers started: the queue fills, then further completions are
	// dropped instead of blocking the watcher.
	done := make(chan struct{})
	go func() {
		for n := 0; n < importQueueSize+5; n++ {
			imp.DeliveryCompleted(progress.Completion{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("DeliveryCompleted blocked on a full queue")
	}
	if len(imp.queue) != importQueueSize {
		t.Errorf("queue holds %d, want %d", len(imp.queue), importQueueSize)
	}
}

func TestIsOwnGrab(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]any
		want  bool
	}{
		{"indexer infohash, other case", map[string]any{domain.TopicExtraSonarrInfoHash: "ABCD"}, true},
		{"history download id only", map[string]any{domain.TopicExtraSonarrDownloadID: "ABCD"}, true},
		{"earlier grab in the set", map[string]any{
			domain.TopicExtraSonarrInfoHash:   "ffff",
			domain.TopicExtraSonarrGrabHashes: []any{"ABCD", "ffff"},
		}, true},
		{"different torrent", map[string]any{domain.TopicExtraSonarrInfoHash: "ffff", domain.TopicExtraSonarrDownloadID: "FFFF"}, false},
		{"nothing recorded", map[string]any{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOwnGrab(tt.extra, "abcd"); got != tt.want {
				t.Errorf("isOwnGrab = %v, want %v", got, tt.want)
			}
		})
	}
}
