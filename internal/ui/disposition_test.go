package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

// C1 (hk:doc decision/2026-09-21/task493-phase1-review): each disposition
// handler refuses a non-email identity itself, before any origin, CSRF, hub or
// store step, so moving a route under /ui/api/ cannot open it to service tokens.
func TestDispositionHandlersRefuseServiceIdentity(t *testing.T) {
	h := &Handler{hub: newHubProxy("", "", nil)}
	handlers := map[string]func(http.ResponseWriter, *http.Request, cfaccess.Identity){
		"answer":       h.answerDisposition,
		"accept-batch": h.acceptDispositionBatch,
		"renotify":     h.renotifyDisposition,
	}
	for name, handle := range handlers {
		for _, identity := range []cfaccess.Identity{{ServiceName: "glance-fixture"}, {}} {
			request := httptest.NewRequest(http.MethodPost, "/ui/dispositions/"+name, strings.NewReader("id=1&gen=1&key=A"))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", "http://example.com")
			recorder := httptest.NewRecorder()
			handle(recorder, request, identity)
			if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "operator identity required") {
				t.Fatalf("%s with %+v: status=%d body=%q", name, identity, recorder.Code, recorder.Body.String())
			}
		}
	}
}

func TestDispositionSnapshotRoundTripAndBounds(t *testing.T) {
	refs, err := parseDispositionSnapshot("3:9,4:11")
	if err != nil || len(refs) != 2 || refs[1].ID != 4 || refs[1].Gen != 11 || encodeDispositionSnapshot(refs) != "3:9,4:11" {
		t.Fatalf("round trip refs=%+v err=%v", refs, err)
	}
	for _, bad := range []string{"", "3", "3:0", "0:1", "a:1", "3:9,,4:1"} {
		if _, err := parseDispositionSnapshot(bad); err == nil {
			t.Errorf("snapshot %q accepted", bad)
		}
	}
	h := &Handler{csrfKey: []byte("k")}
	if h.dispositionSnapshotMAC("a@x", "b", "1", "3:9") == h.dispositionSnapshotMAC("other@x", "b", "1", "3:9") {
		t.Fatal("snapshot token is not bound to the operator email")
	}
}
