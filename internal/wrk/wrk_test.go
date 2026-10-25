package wrk

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestContractCopyIsThePackBundle(t *testing.T) {
	pack, err := os.ReadFile("../../testdata/o3/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pack, contractJSON) {
		t.Fatal("internal/wrk/contract.json differs from testdata/o3/contract.json")
	}
}

func TestCanonicalJSON(t *testing.T) {
	v, err := DecodeStrict([]byte(`{"b":[1,{"z":null,"a":true}],"a":"\u00e9\u2028\n\"","\u20ac":0,"\ud83d\ude00":-7}`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(MustCanonical(v))
	// RFC 8785: UTF-16 code unit order puts U+20AC (0x20AC) before U+1F600
	// (surrogate 0xD83D); only control characters, quote and backslash escape.
	want := "{\"a\":\"\u00e9\u2028\\n\\\"\",\"b\":[1,{\"a\":true,\"z\":null}],\"\u20ac\":0,\"\U0001F600\":-7}"
	if got != want {
		t.Fatalf("canonical\n got %s\nwant %s", got, want)
	}
}

func TestDecodeStrictRefusals(t *testing.T) {
	for _, raw := range []string{
		`{"a":1,"a":1}`, `{"a":1} x`, `{"a":1.0}`, `{"a":1e3}`, `{"a":-0}`, `{"a":01}`,
		`{"a":9007199254740992}`, "{\"a\":\"\xff\"}", ``, `[`,
		`{"a":"\ud800"}`, `{"a":"\udc00"}`, `{"a":"\ud800\u0041"}`, `{"\ud800":1}`, `{"a":"\ud800x"}`, `{"a":"\udbff\ud800"}`, `{"a":"\u12"}`,
	} {
		if _, err := DecodeStrict([]byte(raw)); err == nil {
			t.Errorf("DecodeStrict(%q) accepted", raw)
		}
	}
	if _, err := DecodeStrict([]byte(` {"a":[9007199254740991,-3]} `)); err != nil {
		t.Fatal(err)
	}
	if v, err := DecodeStrict([]byte(`{"\\u":"\ud83d\ude00\\ud800"}`)); err != nil || v.(map[string]any)[`\u`] != "\U0001F600\\ud800" {
		t.Fatalf("valid pair and escaped backslash: %v %v", v, err)
	}
}

func packPolicyBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/o3/vectors/transition.json")
	if err != nil {
		t.Fatal(err)
	}
	var vf struct {
		Cases []struct {
			Given struct {
				Policy json.RawMessage `json:"policy"`
			} `json:"given"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(b, &vf); err != nil {
		t.Fatal(err)
	}
	return vf.Cases[0].Given.Policy
}

// The digest is RFC 8785 of the whole policy: the pack's seeded claims carry
// it, key order and whitespace do not change it, array order does.
func TestPolicyDigest(t *testing.T) {
	raw := packPolicyBytes(t)
	p, err := ParsePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.SHA256() != "8de436a97778f975de840ffd0ec10539440059169fbc34c4a8eb0486f0ece5ce" {
		t.Fatalf("policy digest %s", p.SHA256())
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	indented, _ := json.MarshalIndent(doc, "", "    ")
	if q, err := ParsePolicy(indented); err != nil || q.SHA256() != p.SHA256() {
		t.Fatalf("reformatted policy digest %v %v", q, err)
	}
	perms := doc["clients"].(map[string]any)["desktop"].(map[string]any)["permissions"].([]any)
	perms[0], perms[1] = perms[1], perms[0]
	reordered, _ := json.Marshal(doc)
	if q, err := ParsePolicy(reordered); err != nil || q.SHA256() == p.SHA256() {
		t.Fatal("array order did not change the digest")
	}
}

func mutatePolicy(t *testing.T, f func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(packPolicyBytes(t), &doc); err != nil {
		t.Fatal(err)
	}
	f(doc)
	b, _ := json.Marshal(doc)
	return b
}

func TestPolicyRefusals(t *testing.T) {
	clients := func(doc map[string]any) map[string]any { return doc["clients"].(map[string]any) }
	for name, b := range map[string][]byte{
		"unknown permission grant.any": mutatePolicy(t, func(d map[string]any) {
			c := clients(d)["director-1"].(map[string]any)
			c["permissions"] = append(c["permissions"].([]any), "grant.any")
		}),
		"desktop grant.write": mutatePolicy(t, func(d map[string]any) {
			c := clients(d)["desktop"].(map[string]any)
			c["permissions"] = append(c["permissions"].([]any), "grant.write")
		}),
		"pa-integrator grant.write": mutatePolicy(t, func(d map[string]any) {
			c := clients(d)["pa-integrator"].(map[string]any)
			c["permissions"] = append(c["permissions"].([]any), "grant.write")
		}),
		"desktop mapped to mac-dev": mutatePolicy(t, func(d map[string]any) { clients(d)["desktop"].(map[string]any)["host_client_id"] = "mac-dev" }),
		"director-1 with a host":    mutatePolicy(t, func(d map[string]any) { clients(d)["director-1"].(map[string]any)["host_client_id"] = "desktop" }),
		"missing at-integrator":     mutatePolicy(t, func(d map[string]any) { delete(clients(d), "at-integrator") }),
		"extra client":              mutatePolicy(t, func(d map[string]any) { clients(d)["new-host"] = clients(d)["rpi"] }),
		"head_repeat disabled":      mutatePolicy(t, func(d map[string]any) { d["features"].(map[string]any)["head_repeat_enabled"] = false }),
		"lane cycle":                mutatePolicy(t, func(d map[string]any) { d["lanes"].(map[string]any)["x"] = "y"; d["lanes"].(map[string]any)["y"] = "x" }),
		"missing lane parent":       mutatePolicy(t, func(d map[string]any) { d["lanes"].(map[string]any)["x"] = "nowhere" }),
		"director-1 not a root":     mutatePolicy(t, func(d map[string]any) { d["lanes"].(map[string]any)["director-1"] = "pa-integrator" }),
		"duplicate key":             []byte(strings.Replace(string(packPolicyBytes(t)), `"version":`, `"version":"hk1873.policy.v1","version":`, 1)),
	} {
		if _, err := ParsePolicy(b); err == nil {
			t.Errorf("%s: policy accepted", name)
		} else if strings.Contains(err.Error(), "{") {
			t.Errorf("%s: error echoes content", name)
		}
	}
	// An empty permission list is valid and denies everything.
	p, err := ParsePolicy(mutatePolicy(t, func(d map[string]any) { clients(d)["rpi"].(map[string]any)["permissions"] = []any{} }))
	if err != nil || p.Has("rpi", PermCapabilitiesRead) {
		t.Fatalf("empty rpi permissions: %v", err)
	}
	if p.Known("unknown-host") || p.Has("unknown-host", PermJobRead) {
		t.Fatal("unknown id has permissions")
	}
}

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"/a//b/./c/../d": "/a/b/d", "/../x": "/x", "/A/\u00e9": "/A/\u00e9", "/": "/", "/a/": "/a",
	} {
		if got, ok := NormalizePath(in); !ok || got != want {
			t.Errorf("NormalizePath(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "rel/x", "/a\\b", "/a\x00b", "/a\nb", "/" + strings.Repeat("a", 4096)} {
		if _, ok := NormalizePath(in); ok {
			t.Errorf("NormalizePath(%q) accepted", in)
		}
	}
}

func TestDecodeContentCanonical(t *testing.T) {
	if _, _, ok := DecodeContent("YQ=="); !ok {
		t.Fatal("canonical base64 refused")
	}
	for _, s := range []string{"YQ", "YR==", "Y Q==", "", "===="} {
		if _, _, ok := DecodeContent(s); ok {
			t.Errorf("DecodeContent(%q) accepted", s)
		}
	}
}

func TestDocsExamplePolicyParses(t *testing.T) {
	b, err := os.ReadFile("../../docs/o3-policy.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePolicy(b); err != nil {
		t.Fatalf("docs example policy: %v", err)
	}
	if strings.Contains(strings.ToLower(string(b)), "token") {
		t.Fatal("docs example policy mentions token material")
	}
}
