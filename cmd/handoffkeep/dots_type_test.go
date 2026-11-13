package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDotsRequestTypeAndAdoptionCanvaAreSent(t *testing.T) {
	var seen []intakeRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := intakeRequest{Method: r.Method, Path: r.URL.RequestURI()}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		_, _ = io.WriteString(w, `{"task":{"id":5},"result":{"id":7},"changed":true}`)
	}))
	t.Cleanup(h.Close)

	template := "[목적] 홈 화면\n[유지할 것] 색상\n[범위] 1화면\n[제약] 없음\n[산출물] PNG\n[완료 기준] 채택\n[입력] 커밋 9f14c82, 캡처 환경: 로컬 빌드"
	if _, err := runAgainst(h, "dots", "request", "--title", "Mockup", "--body", template, "--requester-lane", "desk", "--project", "experiment", "--type", "CREATE_MOCKUP"); err != nil {
		t.Fatal(err)
	}
	if got := string(seen[0].Body["type"]); got != `"CREATE_MOCKUP"` {
		t.Fatalf("type = %s, want CREATE_MOCKUP; body=%v", got, seen[0].Body)
	}
	if _, err := runAgainst(h, "dots", "request", "--title", "Research", "--body", "Please investigate", "--requester-lane", "desk", "--project", "experiment"); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen[1].Body["type"]; ok {
		t.Fatalf("untyped request sent a type: %v", seen[1].Body)
	}

	if _, err := runAgainst(h, "dots", "adopt", "7", "--state", "accepted", "--ref", "hk:task/12", "--canva-url", "https://www.canva.com/design/DAF123/edit", "--canva-page", "3", "--canva-version", "v4"); err != nil {
		t.Fatal(err)
	}
	adopt := seen[2]
	if adopt.Method != "POST" || adopt.Path != "/v1/dots/results/7/adoption" {
		t.Fatalf("adopt request = %s %s", adopt.Method, adopt.Path)
	}
	for key, want := range map[string]string{"state": `"accepted"`, "ref": `"hk:task/12"`, "canva_url": `"https://www.canva.com/design/DAF123/edit"`, "canva_page": `"3"`, "canva_version": `"v4"`} {
		if got := string(adopt.Body[key]); got != want {
			t.Fatalf("adopt %s = %s, want %s", key, got, want)
		}
	}
	if !strings.Contains(seen[0].Path, "/v1/dots/requests") {
		t.Fatalf("request path = %s", seen[0].Path)
	}
}
