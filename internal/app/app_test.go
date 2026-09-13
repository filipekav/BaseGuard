package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeExecutor struct {
	Content  string
	Err      error
	CheckErr error
}

func (f fakeExecutor) Check(context.Context, Database) (string, error) {
	return "Conexão OK", f.CheckErr
}
func (f fakeExecutor) Dump(_ context.Context, _ Database, w io.Writer) error {
	_, err := io.WriteString(w, f.Content)
	if err != nil {
		return err
	}
	return f.Err
}
func fixture(t *testing.T) (*Store, *Worker, Database) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "data", "baseguard.db"), filepath.Join(dir, "key", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Close() })
	backup := filepath.Join(dir, "backups")
	if err = InitDestination(backup, "Principal"); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Store: s, Destinations: map[string]string{"Principal": backup}, Executor: fakeExecutor{Content: "valid backup data"}}
	d := DefaultDatabase()
	d.Name = "Produção"
	d.Host = "localhost"
	d.DBName = "app"
	d.Username = "backup"
	d.Password = "secret-test-value"
	d.Destination = "Principal"
	if err = s.Save(&d); err != nil {
		t.Fatal(err)
	}
	return s, w, d
}
func TestEncryptionAndPersistence(t *testing.T) {
	s, _, d := fixture(t)
	var raw string
	if err := s.DB.QueryRow("SELECT config FROM databases WHERE id=?", d.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, d.Password) {
		t.Fatal("plaintext secret persisted")
	}
	read, err := s.Database(d.ID)
	if err != nil || read.Password != d.Password {
		t.Fatalf("round trip: %v", err)
	}
	key := append([]byte(nil), s.Key...)
	key[0] ^= 1
	enc, _ := encrypt(s.Key, "hello")
	if _, err = decrypt(key, enc); err == nil {
		t.Fatal("wrong key accepted")
	}
	dbpath := filepath.Join(t.TempDir(), "existing.db")
	os.WriteFile(dbpath, []byte("existing"), 0600)
	if _, err = loadKey(filepath.Join(t.TempDir(), "missing.key"), dbpath); err == nil {
		t.Fatal("missing key silently replaced")
	}
}
func TestScheduleValidation(t *testing.T) {
	_, w, d := fixture(t)
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	now := time.Date(2026, 9, 13, 1, 0, 0, 0, loc)
	s, err := d.Schedule()
	if err != nil {
		t.Fatal(err)
	}
	if next := s.Next(now); next.Hour() != 2 || next.Day() != 13 {
		t.Fatal(next)
	}
	d.ScheduleKind = "weekly"
	d.Weekday = 1
	s, _ = d.Schedule()
	if s.Next(now).Weekday() != time.Monday {
		t.Fatal("weekday")
	}
	d.ScheduleKind = "interval"
	d.Interval = 3
	s, _ = d.Schedule()
	if s.Next(now).Sub(now) != 3*time.Hour {
		t.Fatal("interval")
	}
	for _, bad := range []string{"../outside", "/etc", "x/../../oops", "C:\\secrets", "a\\..\\b"} {
		d.Subdir = bad
		if d.Validate(w.Destinations) == nil {
			t.Fatalf("accepted path %s", bad)
		}
	}
	d.Subdir = ""
	for _, bad := range []string{"host=evil", "postgresql://evil/db", "--help"} {
		d.DBName = bad
		if d.Validate(w.Destinations) == nil {
			t.Fatalf("accepted dbname %s", bad)
		}
	}
}
func TestQueueConcurrencyAndRecovery(t *testing.T) {
	s, w, d := fixture(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Enqueue(d, "manual", time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	rs, _ := s.Runs()
	if len(rs) != 1 {
		t.Fatalf("duplicate jobs: %d", len(rs))
	}
	r, err := s.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Recover(); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Run(r.ID)
	if r.Status != "interrupted" {
		t.Fatal(r.Status)
	}
	d.Enabled = true
	d.NextRun = time.Now().Add(-72 * time.Hour).Unix()
	if err = s.Save(&d); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = w.Schedule(time.Now(), "recovery"); err != nil {
			t.Fatal(err)
		}
	}
	rs, _ = s.Runs()
	if len(rs) != 2 || rs[0].Trigger != "recovery" {
		t.Fatalf("bad recovery queue: %+v", rs)
	}
	d, _ = s.Database(d.ID)
	if d.NextRun <= time.Now().Unix() {
		t.Fatal("schedule was not advanced")
	}
}
func TestBackupFailureAndRetention(t *testing.T) {
	s, w, d := fixture(t)
	d.Retain = 1
	s.Save(&d)
	run := func() Run {
		t.Helper()
		if _, err := s.Enqueue(d, "manual", time.Now()); err != nil {
			t.Fatal(err)
		}
		r, err := s.Claim()
		if err != nil {
			t.Fatal(err)
		}
		w.execute(context.Background(), r)
		r, _ = s.Run(r.ID)
		return r
	}
	first := run()
	if first.Status != "success" || first.Size == 0 || len(first.SHA256) != 64 {
		t.Fatalf("bad success: %+v", first)
	}
	w.Executor = fakeExecutor{Content: "partial data", Err: errors.New("no space left on device")}
	failed := run()
	if failed.Status != "failed" {
		t.Fatal(failed.Status)
	}
	if _, err := os.Stat(filepath.Join(w.Destinations["Principal"], failed.Path+".partial")); !os.IsNotExist(err) {
		t.Fatal("partial file survived")
	}
	if _, err := os.Stat(filepath.Join(w.Destinations["Principal"], first.Path)); err != nil {
		t.Fatal("failure removed previous backup")
	}
	w.Executor = fakeExecutor{Content: "new valid dump"}
	last := run()
	if last.Status != "success" {
		t.Fatal(last.Error)
	}
	first, _ = s.Run(first.ID)
	if first.Status != "expired" {
		t.Fatal("retention not applied")
	}
	if _, err := os.Stat(filepath.Join(w.Destinations["Principal"], first.Path)); !os.IsNotExist(err) {
		t.Fatal("old file not deleted")
	}
	w.Executor = fakeExecutor{CheckErr: errors.New("invalid credentials")}
	badAuth := run()
	if badAuth.Status != "failed" || !strings.Contains(badAuth.Error, "credentials") {
		t.Fatal(badAuth)
	}
}
func TestDestinationMissingAndEscape(t *testing.T) {
	_, w, d := fixture(t)
	root, err := openDestination(w.Destinations, d.Destination)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, _, err = writeBackup(context.Background(), root, "../escape.dump", d, w.Executor); err == nil {
		t.Fatal("path escaped")
	}
	if err = os.Remove(filepath.Join(w.Destinations[d.Destination], ".baseguard-destination")); err != nil {
		t.Fatal(err)
	}
	if r, err := openDestination(w.Destinations, d.Destination); err == nil {
		r.Close()
		t.Fatal("missing marker accepted")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func TestCommandHelper(t *testing.T) {
	switch os.Getenv("BASEGUARD_TEST_HELPER") {
	case "timeout":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "secret":
		fmt.Fprintln(os.Stderr, "access denied: secret-test-value")
		os.Exit(1)
	case "output":
		fmt.Fprint(os.Stdout, "dump contents")
		os.Exit(0)
	}
}
func TestProcessTimeoutRedactionAndWriteFailure(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := Database{Password: "secret-test-value"}
	for _, mode := range []string{"timeout", "secret", "output"} {
		t.Run(mode, func(t *testing.T) {
		timeout := 5 * time.Second
		if mode == "timeout" { timeout = 300 * time.Millisecond }
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			var out io.Writer = io.Discard
			if mode == "output" {
				out = failWriter{}
			}
			err := command(ctx, d, exe, []string{"-test.run=^TestCommandHelper$"}, append(os.Environ(), "BASEGUARD_TEST_HELPER="+mode), out)
			if err == nil {
				t.Fatal("expected failure")
			}
			if strings.Contains(err.Error(), d.Password) {
				t.Fatal("secret leaked")
			}
			if mode == "timeout" && !strings.Contains(err.Error(), "tempo máximo") {
				t.Fatal(err)
			}
			if mode == "output" && !strings.Contains(err.Error(), "disk full") {
				t.Fatal(err)
			}
		})
	}
	if _, err = exec.LookPath("baseguard-nonexistent-command"); err == nil {
		t.Fatal("unexpected test command")
	}
}

func TestWebAuthenticationCSRFAndDownload(t *testing.T) {
	s, w, d := fixture(t)
	web, err := NewServer(s, w, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = web.Bootstrap("TestPassword2026!"); err != nil {
		t.Fatal(err)
	}
	h := web.Handler()
	request := func(method, path string, values url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		body := ""
		if values != nil {
			body = values.Encode()
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	if r := request("GET", "/", nil); r.Code != 303 {
		t.Fatal("unauthenticated dashboard")
	}
	l := request("GET", "/login", nil)
	lc := l.Result().Cookies()[0]
	if r := request("POST", "/login", url.Values{"username": {"admin"}, "password": {"TestPassword2026!"}}); r.Code != 403 {
		t.Fatal("login CSRF accepted")
	}
	login := request("POST", "/login", url.Values{"username": {"admin"}, "password": {"TestPassword2026!"}, "csrf": {lc.Value}}, lc)
	if login.Code != 303 {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	sc := login.Result().Cookies()[0]
	var token string
	s.DB.QueryRow("SELECT csrf FROM sessions WHERE token=?", tokenHash(sc.Value)).Scan(&token)
	for _, path := range []string{"/", "/status", "/databases/new", fmt.Sprintf("/databases/%d/edit", d.ID), "/settings"} {
		r := request("GET", path, nil, sc)
		if r.Code != 200 || r.Body.Len() < 100 {
			t.Fatalf("page %s: %d", path, r.Code)
		}
		if strings.Contains(r.Body.String(), d.Password) {
			t.Fatal("credential rendered")
		}
	}
	if r := request("POST", fmt.Sprintf("/databases/%d/backup", d.ID), url.Values{}, sc); r.Code != 403 {
		t.Fatal("mutation accepted without CSRF")
	}
	queued := request("POST", fmt.Sprintf("/databases/%d/backup", d.ID), url.Values{"csrf": {token}}, sc)
	if queued.Code != 303 {
		t.Fatal(queued.Code)
	}
	run, err := s.Claim()
	if err != nil {
		t.Fatal(err)
	}
	w.execute(context.Background(), run)
	r := request("GET", fmt.Sprintf("/runs/%d/download", run.ID), nil, sc)
	if r.Code != 200 || r.Body.String() != "valid backup data" {
		t.Fatalf("download: %d %s", r.Code, r.Body.String())
	}
	if r = request("GET", fmt.Sprintf("/runs/%d/download", run.ID), nil); r.Code != 303 {
		t.Fatal("public backup download")
	}
	if r = request("POST", "/logout", url.Values{"csrf": {token}}, sc); r.Code != 303 {
		t.Fatal("logout")
	}
	if r = request("GET", "/", nil, sc); r.Code != 303 {
		t.Fatal("session not revoked")
	}
}

func TestCredentialFilesDoNotLeakInArguments(t *testing.T) {
	_, _, d := fixture(t)
	for _, engine := range []string{"postgres", "mysql"} {
		d.Engine = engine
		env, args, cleanup, err := credentials(d)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(args, " "), d.Password) || strings.Contains(strings.Join(env, " "), d.Password) {
			t.Fatal("password leaked to process arguments or environment")
		}
		cleanup()
	}
}

func TestConnectionEditPreservesSchedulerAdvance(t *testing.T) {
	s, _, d := fixture(t)
	d.Enabled = true
	d.NextRun = time.Now().Add(time.Hour).Unix()
	if err := s.Save(&d); err != nil {
		t.Fatal(err)
	}
	stale, err := s.Database(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	advanced := d.NextRun + 86400
	if _, err = s.DB.Exec("UPDATE databases SET next_run=? WHERE id=?", advanced, d.ID); err != nil {
		t.Fatal(err)
	}
	stale.Name = "Renamed"
	if err = s.Save(&stale); err != nil {
		t.Fatal(err)
	}
	actual, _ := s.Database(d.ID)
	if actual.NextRun != advanced {
		t.Fatal("connection edit undid scheduler tick")
	}
}

func TestPersistentQueueSurvivesStoreReopen(t *testing.T) {
	dir := t.TempDir()
	dbpath, keypath := filepath.Join(dir, "baseguard.db"), filepath.Join(dir, "master.key")
	s, err := OpenStore(dbpath, keypath)
	if err != nil {
		t.Fatal(err)
	}
	d := DefaultDatabase()
	d.Name = "Persistent"
	d.Password = "secret"
	if err = s.Save(&d); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue(d, "manual", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dbpath, keypath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	r, err := s.Claim()
	if err != nil || r.DatabaseID != d.ID {
		t.Fatalf("queue lost: %+v %v", r, err)
	}
	got, err := s.Database(d.ID)
	if err != nil || got.Password != "secret" {
		t.Fatalf("credentials lost: %v", err)
	}
}

func TestRunClaimIsExclusiveAndMissingDBNotQueued(t *testing.T) {
	s, _, d := fixture(t)
	s.Enqueue(d, "manual", time.Now())
	var wg sync.WaitGroup
	results := make(chan int64, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := s.Claim(); err == nil {
				results <- r.ID
			}
		}()
	}
	wg.Wait()
	close(results)
	if len(results) != 1 {
		t.Fatalf("claimed %d times", len(results))
	}
	d.ID = 999
	if ok, err := s.Enqueue(d, "manual", time.Now()); ok || err != nil {
		t.Fatalf("queued deleted database: %v %v", ok, err)
	}
}

func TestWebDatabaseLifecycle(t *testing.T) {
	s, w, d := fixture(t)
	web, err := NewServer(s, w, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec("INSERT INTO sessions VALUES(?,?,?)", tokenHash("test-session"), "test-csrf", time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	h := web.Handler()
	post := func(path string, values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		values.Set("csrf", "test-csrf")
		req := httptest.NewRequest("POST", path, strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "baseguard_session", Value: "test-session"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	values := url.Values{"name": {"Novo banco"}, "engine": {"postgres"}, "host": {"localhost"}, "port": {"5432"}, "dbname": {"app"}, "username": {"backup"}, "password": {"new-secret"}, "tls": {"require"}, "destination": {"Principal"}, "schedule": {"daily"}, "at": {"02:00"}, "timezone": {"America/Sao_Paulo"}, "interval": {"24"}, "weekday": {"0"}, "retain": {"7"}, "timeout": {"120"}}
	if rec := post("/databases/save", values); rec.Code != 303 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	dbs, _ := s.Databases()
	created := dbs[0]
	if created.Enabled || created.Name != "Novo banco" {
		t.Fatal("wrong defaults")
	}
	values.Set("id", fmt.Sprint(created.ID))
	values.Set("name", "Editado")
	values.Set("password", "")
	if rec := post("/databases/save", values); rec.Code != 303 {
		t.Fatal("edit")
	}
	created, _ = s.Database(created.ID)
	if created.Password != "new-secret" {
		t.Fatal("blank edit lost password")
	}
	if rec := post(fmt.Sprintf("/databases/%d/toggle", created.ID), url.Values{}); rec.Code != 303 {
		t.Fatal("toggle")
	}
	created, _ = s.Database(created.ID)
	if !created.Enabled || created.NextRun <= time.Now().Unix() {
		t.Fatal("schedule not enabled")
	}
	if rec := post("/databases/test", values); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Conexão OK") {
		t.Fatal("connection response")
	}
	values.Set("subdir", "../outside")
	if rec := post("/databases/save", values); rec.Code != 422 {
		t.Fatal("unsafe form accepted")
	}
	s.Enqueue(d, "manual", time.Now())
	post(fmt.Sprintf("/databases/%d/delete", d.ID), url.Values{})
	if _, err = s.Database(d.ID); err != nil {
		t.Fatal("deleted active database")
	}
	post(fmt.Sprintf("/databases/%d/delete", created.ID), url.Values{})
	if _, err = s.Database(created.ID); err == nil {
		t.Fatal("delete failed")
	}
}
