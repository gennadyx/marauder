package progress

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/artyomsv/marauder/backend/internal/domain"
	"github.com/artyomsv/marauder/backend/internal/plugins/registry"
)

type recordingHook struct {
	calls []Completion
	panic bool
}

func (h *recordingHook) DeliveryCompleted(c Completion) {
	h.calls = append(h.calls, c)
	if h.panic {
		panic("hook bug")
	}
}

// fakeFileStatus is a status client that can also list files.
type fakeFileStatus struct {
	fakeStatus
	files []domain.ClientFile
}

func (f fakeFileStatus) Files(context.Context, []byte, string) ([]domain.ClientFile, error) {
	return f.files, nil
}
func (f fakeFileStatus) SkipFiles(context.Context, []byte, string, []int) error { return nil }
func (f fakeFileStatus) Start(context.Context, []byte, string) error            { return nil }

func TestPoll_WonCompletion_CallsHookOnce(t *testing.T) {
	cid := uuid.New()
	d := inflight(cid)
	del := &fakeDeliveries{inflight: []*domain.InFlightDelivery{d}, markWon: true}
	st := fakeStatus{statuses: []registry.TorrentStatus{{Hash: "abc123", PercentDone: 1, State: registry.StateSeeding, SavePath: "/dl"}}}
	w := newTestWatcher(t, del, &fakeEmitter{}, st)
	hook := &recordingHook{}
	w.SetCompletionHook(hook)

	w.poll(context.Background())
	if len(hook.calls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(hook.calls))
	}
	if hook.calls[0].Delivery.DeliveryID != d.DeliveryID {
		t.Errorf("hook got delivery %v, want %v", hook.calls[0].Delivery.DeliveryID, d.DeliveryID)
	}

	insp, err := hook.calls[0].Inspect(context.Background())
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !insp.Found || insp.Status.SavePath != "/dl" {
		t.Errorf("inspect = %+v, want found with save path /dl", insp)
	}
	if insp.FilesSupported || insp.Files != nil {
		t.Errorf("a client without file selection must report FilesSupported=false: %+v", insp)
	}
}

func TestPoll_LostCompletion_DoesNotCallHook(t *testing.T) {
	cid := uuid.New()
	del := &fakeDeliveries{inflight: []*domain.InFlightDelivery{inflight(cid)}, markWon: false}
	st := fakeStatus{statuses: []registry.TorrentStatus{{Hash: "abc123", PercentDone: 1, State: registry.StateSeeding}}}
	w := newTestWatcher(t, del, &fakeEmitter{}, st)
	hook := &recordingHook{}
	w.SetCompletionHook(hook)
	w.poll(context.Background())
	if len(hook.calls) != 0 {
		t.Fatalf("a lost NULL→now transition must not reach the hook: %d calls", len(hook.calls))
	}
}

func TestPoll_HookPanic_IsContained(t *testing.T) {
	cid := uuid.New()
	del := &fakeDeliveries{inflight: []*domain.InFlightDelivery{inflight(cid)}, markWon: true}
	st := fakeStatus{statuses: []registry.TorrentStatus{{Hash: "abc123", PercentDone: 1, State: registry.StateSeeding}}}
	emit := &fakeEmitter{}
	w := newTestWatcher(t, del, emit, st)
	w.SetCompletionHook(&recordingHook{panic: true})
	w.poll(context.Background()) // must not panic
	if len(del.completed) != 1 || len(emit.events) != 1 {
		t.Fatalf("completion must stand despite the hook panic: completed=%v events=%d", del.completed, len(emit.events))
	}
}

func TestInspector_ReadsFilesWhenSupported(t *testing.T) {
	plugin := fakeFileStatus{
		fakeStatus: fakeStatus{statuses: []registry.TorrentStatus{{Hash: "ABC123", PercentDone: 1, State: registry.StateSeeding, SavePath: "/dl"}}},
		files:      []domain.ClientFile{{Index: 0, Path: "Show/E01.mkv", Size: 10, Wanted: true}},
	}
	insp, err := inspector(plugin, nil, "abc123")(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !insp.Found || !insp.FilesSupported || len(insp.Files) != 1 || insp.Files[0].Path != "Show/E01.mkv" {
		t.Errorf("inspect = %+v", insp)
	}
}

func TestInspector_TorrentGone(t *testing.T) {
	plugin := fakeFileStatus{fakeStatus: fakeStatus{}}
	insp, err := inspector(plugin, nil, "abc123")(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if insp.Found || insp.FilesSupported {
		t.Errorf("a torrent the client forgot must be Found=false without a file read: %+v", insp)
	}
}

// failingStatus is a status client whose Status fails with err.
type failingStatus struct {
	fakeStatus
	err error
}

func (f failingStatus) Status(context.Context, []byte, []string) ([]registry.TorrentStatus, error) {
	return nil, f.err
}

func TestInspector_SanitisesClientErrors(t *testing.T) {
	secretURL := "http://admin:hunter2@qbit:8080/api/v2/torrents/info?hashes=abc"
	tests := []struct {
		name    string
		err     error
		want    string
		wantCtx error
	}{
		{"transport error carries the URL", &url.Error{Op: "Get", URL: secretURL, Err: errors.New("connection refused")},
			"status: torrent client unreachable", nil},
		{"status error carries the body", errors.New("qbit status 500: <html>secret page</html>"),
			"status: torrent client returned an error", nil},
		{"timeout stays matchable", &url.Error{Op: "Get", URL: secretURL, Err: context.DeadlineExceeded},
			"status: torrent client timed out", context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := inspector(failingStatus{err: tt.err}, nil, "abc")(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			for _, leak := range []string{"hunter2", "qbit:8080", "/api/v2", "secret page"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error leaks %q: %v", leak, err)
				}
			}
			if !strings.HasPrefix(err.Error(), tt.want) {
				t.Errorf("error = %q, want prefix %q", err, tt.want)
			}
			if tt.wantCtx != nil && !errors.Is(err, tt.wantCtx) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.wantCtx)
			}
		})
	}
}

func TestClientHost_OnlyTheHostLeaves(t *testing.T) {
	tests := []struct{ raw, want string }{
		{`{"url":"http://admin:pw@QBit:8080/","password":"x"}`, "qbit"},
		{`{"url":"http://transmission:9091/transmission/rpc"}`, "transmission"},
		{`{"url":"http://[::1]:9091/"}`, "::1"},
		{`{"url":""}`, ""},
		{`not json`, ""},
	}
	for _, tt := range tests {
		if got := clientHost([]byte(tt.raw)); got != tt.want {
			t.Errorf("clientHost(%s) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}

func TestPoll_HookGetsTheClientHost(t *testing.T) {
	cid := uuid.New()
	del := &fakeDeliveries{inflight: []*domain.InFlightDelivery{inflight(cid)}, markWon: true}
	st := fakeStatus{statuses: []registry.TorrentStatus{{Hash: "abc123", PercentDone: 1, State: registry.StateSeeding}}}
	w := newTestWatcher(t, del, &fakeEmitter{}, st)
	// fakeDecryptor hands ConfigEnc back as the decrypted config.
	w.clients = &fakeClients{client: &domain.Client{ID: cid, ClientName: "fake",
		ConfigEnc: []byte(`{"url":"http://user:pass@Transmission:9091/transmission/rpc"}`)}}
	hook := &recordingHook{}
	w.SetCompletionHook(hook)
	w.poll(context.Background())
	if len(hook.calls) != 1 || hook.calls[0].ClientHost != "transmission" {
		t.Fatalf("hook calls = %+v, want ClientHost transmission", hook.calls)
	}
}
