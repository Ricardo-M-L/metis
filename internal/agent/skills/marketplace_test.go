package skills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHash_StableExcludingUses(t *testing.T) {
	a := &Skill{Name: "n", Description: "d", Prompt: "p", Uses: 0}
	b := &Skill{Name: "n", Description: "d", Prompt: "p", Uses: 99}
	if Hash(a) != Hash(b) {
		t.Error("hash should ignore Uses field")
	}
}

func TestHash_ChangesWithContent(t *testing.T) {
	a := &Skill{Name: "n", Prompt: "p"}
	b := &Skill{Name: "n", Prompt: "different"}
	if Hash(a) == Hash(b) {
		t.Error("hash should change when prompt changes")
	}
}

func TestVerify_EmptyHashAccepted(t *testing.T) {
	if err := Verify(&Skill{Name: "n"}); err != nil {
		t.Errorf("empty hash should be accepted (unsigned), got %v", err)
	}
}

func TestVerify_GoodHash(t *testing.T) {
	s := &Skill{Name: "n", Prompt: "p"}
	s.ContentHash = Hash(s)
	if err := Verify(s); err != nil {
		t.Errorf("matching hash should verify, got %v", err)
	}
}

func TestVerify_TamperedHash(t *testing.T) {
	s := &Skill{Name: "n", Prompt: "p"}
	s.ContentHash = Hash(s)
	s.Prompt = "tampered"
	if err := Verify(s); err == nil {
		t.Error("tampered prompt should fail hash verification")
	}
}

func TestParseGitHubRef(t *testing.T) {
	cases := []struct {
		in              string
		owner, repo, n  string
		err             bool
	}{
		{"foo/bar:baz", "foo", "bar", "baz", false},
		{"a/b:c-d", "a", "b", "c-d", false},
		{"no-slash", "", "", "", true},
		{"a/b", "", "", "", true},
		{"/x:y", "", "", "", true},
		{"a/:y", "", "", "", true},
	}
	for _, c := range cases {
		o, r, n, err := parseGitHubRef(c.in)
		if (err != nil) != c.err {
			t.Errorf("parse(%q) err = %v, wantErr=%v", c.in, err, c.err)
			continue
		}
		if c.err {
			continue
		}
		if o != c.owner || r != c.repo || n != c.n {
			t.Errorf("parse(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, o, r, n, c.owner, c.repo, c.n)
		}
	}
}

func TestGitHubSource_Fetch(t *testing.T) {
	want := &Skill{Name: "demo", Description: "d", Prompt: "p"}
	want.ContentHash = Hash(want)
	body, _ := json.Marshal(want)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/main/skills/demo.json") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Write(body)
	}))
	defer srv.Close()

	src := &GitHubSource{HTTPClient: srv.Client(), BaseURL: srv.URL}
	got, err := src.Fetch(context.Background(), "owner/repo:demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "d" {
		t.Errorf("manifest mismatch: %+v", got)
	}
	if got.Source != "github:owner/repo:demo" {
		t.Errorf("source not set: %q", got.Source)
	}
}

func TestStore_InstallVerifiesHash(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	tampered := &Skill{Name: "x", Prompt: "real"}
	tampered.ContentHash = Hash(tampered)
	tampered.Prompt = "tampered after sign"
	body, _ := json.Marshal(tampered)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	src := &GitHubSource{HTTPClient: srv.Client(), BaseURL: srv.URL}
	if _, err := store.Install(context.Background(), src, "o/r:x"); err == nil {
		t.Error("tampered skill should fail to install")
	}
}

func TestStore_InstallSavesGoodSkill(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	good := &Skill{Name: "ok", Description: "good", Prompt: "p"}
	good.ContentHash = Hash(good)
	body, _ := json.Marshal(good)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	src := &GitHubSource{HTTPClient: srv.Client(), BaseURL: srv.URL}
	saved, err := store.Install(context.Background(), src, "o/r:ok")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Source != "github:o/r:ok" {
		t.Errorf("source mismatch: %q", saved.Source)
	}
	got, err := store.Get("ok")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "good" {
		t.Errorf("saved skill mismatch: %+v", got)
	}
}
