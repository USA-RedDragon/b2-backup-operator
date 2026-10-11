package borgbase

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"

	"k8s.io/utils/ptr"
)

type stub struct {
	response string
	requests []map[string]any
	authz    string
}

func (s *stub) server(t *testing.T) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.authz = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		s.requests = append(s.requests, req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, s.response)
	}))
	c := NewClient("test-token")
	c.Endpoint = srv.URL
	return c, srv.Close
}

func (s *stub) lastQuery() string {
	if len(s.requests) == 0 {
		return ""
	}
	q, _ := s.requests[len(s.requests)-1]["query"].(string)
	return q
}

func (s *stub) lastVars() map[string]any {
	if len(s.requests) == 0 {
		return nil
	}
	v, _ := s.requests[len(s.requests)-1]["variables"].(map[string]any)
	return v
}

func TestGetSendsBearerTokenAndParsesRepo(t *testing.T) {
	s := &stub{response: `{"data":{"repo":{
		"id":"a1b2c3d4","name":"myapp-prod","region":"us","format":"restic",
		"repoPath":"a1b2c3d4@a1b2c3d4.repo.borgbase.com:repo",
		"quota":100,"quotaEnabled":true,"alertDays":3,"appendOnly":false,
		"currentUsage":3.25,"htpasswd":"rest-secret",
		"server":{"hostname":"box-us00.borgbase.com","region":"us"}}}}`}
	c, done := s.server(t)
	defer done()

	repo, err := c.Get(context.Background(), "a1b2c3d4")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if s.authz != "Bearer test-token" {
		t.Errorf("Authorization = %q", s.authz)
	}
	if repo.ID != "a1b2c3d4" || !repo.IsRestic() || repo.CurrentUsage != 3.25 {
		t.Errorf("Get() = %+v", repo)
	}

	url, err := repo.ResticURL()
	if err != nil {
		t.Fatalf("ResticURL() error = %v", err)
	}
	want := "rest:https://a1b2c3d4:rest-secret@a1b2c3d4.repo.borgbase.com"
	if url != want {
		t.Errorf("ResticURL() = %q, want %q", url, want)
	}
}

func TestGetMapsNotFound(t *testing.T) {
	s := &stub{response: `{"errors":[{"message":"Repository not found"}]}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestGetNullRepoIsNotFound(t *testing.T) {
	s := &stub{response: `{"data":{"repo":null}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestAddAlwaysRequestsResticFormat(t *testing.T) {
	s := &stub{response: `{"data":{"repoAdd":{"repoAdded":{
		"id":"new12345","name":"myapp-prod","region":"us","format":"restic",
		"htpasswd":"t","server":{"hostname":"new12345.repo.borgbase.com"}}}}}`}
	c, done := s.server(t)
	defer done()

	quota := int64(100)
	if _, err := c.Add(context.Background(), AddOptions{
		Name: "myapp-prod", Region: "us", Quota: &quota,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	vars := s.lastVars()
	if vars["format"] != FormatRestic {
		t.Errorf("format = %v, want %q", vars["format"], FormatRestic)
	}
	if vars["quotaEnabled"] != true {
		t.Errorf("quotaEnabled = %v, want true when a quota is set", vars["quotaEnabled"])
	}
}

func TestAddOmitsQuotaWhenUnset(t *testing.T) {
	s := &stub{response: `{"data":{"repoAdd":{"repoAdded":{
		"id":"new12345","format":"restic","htpasswd":"t","server":{"hostname":"h"}}}}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Add(context.Background(), AddOptions{Name: "n", Region: "us"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	vars := s.lastVars()
	if _, present := vars["quota"]; present {
		t.Error("quota must be omitted entirely when unset")
	}
	if vars["quotaEnabled"] != false {
		t.Errorf("quotaEnabled = %v, want false", vars["quotaEnabled"])
	}
}

func TestEditNeverSendsFormat(t *testing.T) {
	s := &stub{response: `{"data":{"repoEdit":{"repoEdited":{
		"id":"a1b2c3d4","format":"restic","htpasswd":"t","server":{"hostname":"h"}}}}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Edit(context.Background(), "a1b2c3d4", EditOptions{
		AppendOnly: ptr.To(true),
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}

	if strings.Contains(s.lastQuery(), "$format") {
		t.Error("repoEdit must not pass format; it is fixed at creation")
	}
	if _, present := s.lastVars()["format"]; present {
		t.Error("repoEdit must not send a format variable")
	}
}

func TestFindByNameReturnsTheOneExactMatch(t *testing.T) {
	s := &stub{response: `{"data":{"repoList":[
		{"id":"aaa","name":"myapp-prod-old","format":"restic"},
		{"id":"bbb","name":"myapp-prod","format":"restic","htpasswd":"rest-secret",
		 "quota":100,"quotaEnabled":true,"currentUsage":1.5},
		{"id":"ccc","name":"myapp-staging","format":"restic"}]}}`}
	c, done := s.server(t)
	defer done()

	repo, err := c.FindByName(context.Background(), "myapp-prod")
	if err != nil {
		t.Fatalf("FindByName() error = %v", err)
	}
	if repo.ID != "bbb" {
		t.Fatalf("FindByName() = %q, want the exactly matching repository bbb", repo.ID)
	}
	if repo.Htpasswd != "rest-secret" || !repo.QuotaEnabled || repo.CurrentUsage != 1.5 {
		t.Errorf("FindByName() returned a partly decoded repository: %+v", repo)
	}
	if name, _ := s.lastVars()["name"].(string); name != "myapp-prod" {
		t.Errorf("repoList name variable = %q, want the name being looked up", name)
	}
}

func TestFindByNameRefusesAmbiguousMatches(t *testing.T) {
	s := &stub{response: `{"data":{"repoList":[
		{"id":"aaa","name":"myapp-prod","format":"restic"},
		{"id":"bbb","name":"myapp-prod","format":"restic"}]}}`}
	c, done := s.server(t)
	defer done()

	_, err := c.FindByName(context.Background(), "myapp-prod")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByName() error = %v, want an ambiguity error", err)
	}
}

func TestFindByNameRequiresExactMatch(t *testing.T) {
	s := &stub{response: `{"data":{"repoList":[{"id":"aaa","name":"myapp-prod-old","format":"restic"}]}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.FindByName(context.Background(), "myapp-prod"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindByName() error = %v, want ErrNotFound", err)
	}
}

func TestResticURLRejectsUnusableRepos(t *testing.T) {
	if _, err := (&Repo{ID: "x", Format: "borg"}).ResticURL(); !errors.Is(err, ErrNotRestic) {
		t.Error("a borg repository must not yield a restic URL")
	}
	if _, err := (&Repo{ID: "x", Format: FormatRestic}).ResticURL(); !errors.Is(err, ErrNoCredentials) {
		t.Error("a repository with no REST password must not yield a URL")
	}
}

func TestPasswordIsTheHtpasswd(t *testing.T) {
	if got := (&Repo{Htpasswd: "h"}).Password(); got != "h" {
		t.Errorf("Password() = %q, want the htpasswd", got)
	}

	url, err := (&Repo{ID: "abc123", Format: FormatRestic, Htpasswd: "h"}).ResticURL()
	if err != nil {
		t.Fatalf("ResticURL() error = %v", err)
	}
	if want := "rest:https://abc123:h@abc123.repo.borgbase.com"; url != want {
		t.Errorf("ResticURL() = %q, want %q", url, want)
	}

	if _, err := (&Repo{ID: "x", Format: FormatRestic}).ResticURL(); !errors.Is(err, ErrNoCredentials) {
		t.Error("a repository with no password must not yield a URL")
	}
}

func TestHostAlwaysUsesTheRepoSubdomain(t *testing.T) {
	bare := &Repo{ID: "abc123"}
	if got := bare.Host(); got != bare.ID+".repo.borgbase.com" {
		t.Errorf("Host() = %q", got)
	}

	onBox := &Repo{
		ID: "e5f6a7b8", Format: FormatRestic, Htpasswd: "tok",
		Server: Server{Hostname: "box-us00.borgbase.com"},
	}
	if got := onBox.Host(); got != onBox.ID+".repo.borgbase.com" {
		t.Errorf("Host() = %q, want the repo subdomain, not the storage box", got)
	}
	url, err := onBox.ResticURL()
	if err != nil {
		t.Fatal(err)
	}
	if want := "rest:https://e5f6a7b8:tok@e5f6a7b8.repo.borgbase.com"; url != want {
		t.Errorf("ResticURL() = %q, want %q", url, want)
	}
}

func TestNonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient("bad")
	c.Endpoint = srv.URL

	if _, err := c.Get(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestEditOmitsUnsetFields(t *testing.T) {
	s := &stub{response: `{"data":{"repoEdit":{"repoEdited":{
		"id":"a1b2c3d4","format":"restic","htpasswd":"t","server":{"hostname":"h"}}}}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Edit(context.Background(), "a1b2c3d4", EditOptions{
		AlertDays: ptr.To(int64(3)),
	}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}

	vars := s.lastVars()
	if vars["alertDays"] != float64(3) {
		t.Errorf("alertDays = %v, want 3", vars["alertDays"])
	}
	for _, absent := range []string{"quota", "quotaEnabled", "appendOnly"} {
		if _, present := vars[absent]; present {
			t.Errorf("%s was sent despite being unset", absent)
		}
	}
}

func TestDeleteChecksOK(t *testing.T) {
	s := &stub{response: `{"data":{"repoDelete":{"ok":false}}}`}
	c, done := s.server(t)
	defer done()

	if err := c.Delete(context.Background(), "a1b2c3d4"); err == nil {
		t.Fatal("expected an error when repoDelete reports ok=false")
	}

	s.response = `{"data":{"repoDelete":{"ok":true}}}`
	if err := c.Delete(context.Background(), "a1b2c3d4"); err != nil {
		t.Errorf("Delete() error = %v, want success for ok=true", err)
	}
}

func TestNotFoundHeuristicIsNarrow(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{"Repository not found", true},
		{"repo does not exist", true},
		{"No repository found with that id", true},
		{"Region does not exist", false},
		{"Server not found", false},
		{"Invalid token", false},
	}
	for _, tt := range tests {
		s := &stub{response: `{"errors":[{"message":"` + tt.message + `"}]}`}
		c, done := s.server(t)
		_, err := c.Get(context.Background(), "x")
		done()

		if got := errors.Is(err, ErrNotFound); got != tt.want {
			t.Errorf("%q: ErrNotFound = %v, want %v (err = %v)", tt.message, got, tt.want, err)
		}
	}
}

func TestNonOKStatusIncludesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "token lacks repoAdd scope")
	}))
	defer srv.Close()
	c := NewClient("bad")
	c.Endpoint = srv.URL

	_, err := c.Get(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "repoAdd scope") {
		t.Errorf("error = %v, want it to carry the response body", err)
	}
}

func TestResticURLSurvivesAReservedCharacterPassword(t *testing.T) {
	for _, pass := range []string{
		"p@ss/w:rd#1",
		"s3cret%2Fx",
		"a?b#c/d",
		"plain16charsxx00",
	} {
		repo := &Repo{ID: "e5f6a7b8", Format: FormatRestic, Htpasswd: pass}
		raw, err := repo.ResticURL()
		if err != nil {
			t.Fatalf("%q: ResticURL() error = %v", pass, err)
		}

		parsed, err := neturl.Parse(strings.TrimPrefix(raw, "rest:"))
		if err != nil {
			t.Errorf("%q: restic cannot parse %q: %v", pass, raw, err)
			continue
		}
		if parsed.Host != repo.Host() {
			t.Errorf("%q: parsed host = %q, want %q", pass, parsed.Host, repo.Host())
		}
		if parsed.User.Username() != repo.ID {
			t.Errorf("%q: parsed user = %q, want %q", pass, parsed.User.Username(), repo.ID)
		}
		got, _ := parsed.User.Password()
		if got != pass {
			t.Errorf("%q: password round-tripped as %q; the wrong password goes on the wire", pass, got)
		}
	}
}

func TestDeleteRequiresAnAffirmativeOK(t *testing.T) {
	// MutationResult.ok is Boolean! in the BorgBase schema, so a success always
	// carries it and a missing one is never a delete this operator may report.
	for _, response := range []string{
		`{"data":{"repoDelete":{"ok":null}}}`,
		`{"data":{"repoDelete":{}}}`,
		`{"data":{}}`,
	} {
		s := &stub{response: response}
		c, done := s.server(t)
		err := c.Delete(context.Background(), "a1b2c3d4")
		done()

		if err == nil {
			t.Errorf("Delete() with %s returned success without an affirmative ok", response)
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("Delete() with %s reported the repository as absent", response)
		}
	}
}
