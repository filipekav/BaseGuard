package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web/*
var webFiles embed.FS

type sessionKey struct{}
type session struct{ CSRF string }
type attempt struct {
	Count int
	Since time.Time
}
type Server struct {
	Store         *Store
	Worker        *Worker
	SecureCookies bool
	templates     *template.Template
	mu            sync.Mutex
	attempts      map[string]attempt
}

func NewServer(s *Store, w *Worker, secure bool) (*Server, error) {
	t, err := template.New("").Funcs(template.FuncMap{
		"nextDate": func(d Database) string {
			if d.NextRun == 0 {
				return "—"
			}
			loc, err := time.LoadLocation(d.Timezone)
			if err != nil {
				return "—"
			}
			return time.Unix(d.NextRun, 0).In(loc).Format("02/01 15:04")
		},
		"date": displayTime, "bytes": func(n int64) string {
			if n < 1024 {
				return fmt.Sprintf("%d B", n)
			}
			if n < 1024*1024 {
				return fmt.Sprintf("%.1f KB", float64(n)/1024)
			}
			if n < 1024*1024*1024 {
				return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
			}
			return fmt.Sprintf("%.2f GB", float64(n)/(1024*1024*1024))
		},
		"status": func(s string) string {
			if v, ok := map[string]string{"queued": "Na fila", "running": "Em andamento", "success": "Concluído", "failed": "Falhou", "interrupted": "Interrompido", "expired": "Expirado"}[s]; ok {
				return v
			}
			return s
		},
		"trigger": func(s string) string {
			if v, ok := map[string]string{"manual": "Manual", "scheduled": "Agendado", "recovery": "Recuperação"}[s]; ok {
				return v
			}
			return s
		},
		"duration": func(r Run) string {
			if r.Started == 0 {
				return "—"
			}
			end := r.Finished
			if end == 0 {
				end = time.Now().Unix()
			}
			return (time.Duration(end-r.Started) * time.Second).String()
		},
		"schedule": func(d Database) string {
			switch d.ScheduleKind {
			case "interval":
				return fmt.Sprintf("A cada %d h", d.Interval)
			case "weekly":
				return []string{"Dom", "Seg", "Ter", "Qua", "Qui", "Sex", "Sáb"}[d.Weekday] + " às " + d.At
			}
			return "Diário às " + d.At
		},
	}).ParseFS(webFiles, "web/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{Store: s, Worker: w, SecureCookies: secure, templates: t, attempts: make(map[string]attempt)}, nil
}

func (s *Server) Bootstrap(password string) (string, error) {
	_, err := s.Store.Setting("admin_hash")
	if err == nil {
		return "", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	generated := ""
	if password == "" {
		password = randomToken()
		generated = password
	}
	if len(password) < 12 || len(password) > 72 {
		return "", fmt.Errorf("senha inicial deve ter de 12 a 72 bytes")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	_, err = s.Store.DB.Exec("INSERT INTO settings(key,value) VALUES('admin_hash',?)", string(h))
	return generated, err
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(webFiles, "web")
	staticFiles := http.StripPrefix("/static/", http.FileServer(http.FS(assets)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/static/style.css", "/static/app.js", "/static/htmx.min.js", "/static/icon.svg", "/static/HTMX-LICENSE.txt":
			staticFiles.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "unhealthy", 503)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("GET /{$}", s.auth(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /status", s.auth(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /databases/new", s.auth(http.HandlerFunc(s.form)))
	mux.Handle("GET /databases/{id}/edit", s.auth(http.HandlerFunc(s.form)))
	mux.Handle("POST /databases/save", s.auth(http.HandlerFunc(s.save)))
	mux.Handle("POST /databases/test", s.auth(http.HandlerFunc(s.test)))
	mux.Handle("POST /databases/{id}/backup", s.auth(http.HandlerFunc(s.enqueue)))
	mux.Handle("POST /databases/{id}/toggle", s.auth(http.HandlerFunc(s.toggle)))
	mux.Handle("POST /databases/{id}/delete", s.auth(http.HandlerFunc(s.delete)))
	mux.Handle("GET /runs/{id}/download", s.auth(http.HandlerFunc(s.download)))
	mux.Handle("GET /settings", s.auth(http.HandlerFunc(s.settings)))
	mux.Handle("POST /password", s.auth(http.HandlerFunc(s.password)))
	mux.Handle("POST /logout", s.auth(http.HandlerFunc(s.logout)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method == "POST" {
			r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
			origin := r.Header.Get("Origin")
			if origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					http.Error(w, "Origem inválida", 403)
					return
				}
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) cookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: age})
}
func equal(a, b string) bool { return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("baseguard_session")
		var csrf string
		if err == nil {
			err = s.Store.DB.QueryRow("SELECT csrf FROM sessions WHERE token=? AND expires>?", tokenHash(c.Value), time.Now().Unix()).Scan(&csrf)
		}
		if err != nil {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(401)
			} else {
				http.Redirect(w, r, "/login", 303)
			}
			return
		}
		if r.Method == "POST" {
			if err = r.ParseForm(); err != nil {
				http.Error(w, "Formulário inválido", 400)
				return
			}
			if !equal(csrf, r.FormValue("csrf")) {
				http.Error(w, "Sessão do formulário inválida; recarregue a página", 403)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, session{CSRF: csrf})))
	})
}
func csrf(r *http.Request) string { return r.Context().Value(sessionKey{}).(session).CSRF }
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "error", err)
	}
}
func (s *Server) failure(w http.ResponseWriter, err error) {
	slog.Error("painel", "error", err)
	http.Error(w, "Não foi possível concluir a operação. Consulte os logs.", 500)
}
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), 303)
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	token := randomToken()
	s.cookie(w, "baseguard_login", token, 600)
	s.render(w, "login", Dashboard{CSRF: token})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Formulário inválido", 400)
		return
	}
	c, err := r.Cookie("baseguard_login")
	if err != nil || !equal(c.Value, r.FormValue("csrf")) {
		http.Error(w, "Recarregue a página de login", 403)
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	s.mu.Lock()
	for k, a := range s.attempts {
		if now.Sub(a.Since) > 15*time.Minute {
			delete(s.attempts, k)
		}
	}
	a := s.attempts[ip]
	if a.Count == 0 {
		a.Since = now
	}
	a.Count++
	s.attempts[ip] = a
	blocked := a.Count > 10 || len(s.attempts) > 1000
	s.mu.Unlock()
	if blocked {
		w.Header().Set("Retry-After", "900")
		http.Error(w, "Muitas tentativas. Aguarde 15 minutos.", 429)
		return
	}
	h, err := s.Store.Setting("admin_hash")
	if err != nil {
		s.failure(w, err)
		return
	}
	err = bcrypt.CompareHashAndPassword([]byte(h), []byte(r.FormValue("password")))
	if err != nil || r.FormValue("username") != "admin" {
		w.WriteHeader(401)
		s.render(w, "login", Dashboard{CSRF: c.Value, Error: "Usuário ou senha incorretos."})
		return
	}
	token := randomToken()
	_, _ = s.Store.DB.Exec("DELETE FROM sessions WHERE expires<?", now.Unix())
	_, err = s.Store.DB.Exec("INSERT INTO sessions(token,csrf,expires) VALUES(?,?,?)", tokenHash(token), randomToken(), now.Add(12*time.Hour).Unix())
	if err != nil {
		s.failure(w, err)
		return
	}
	s.mu.Lock()
	delete(s.attempts, ip)
	s.mu.Unlock()
	s.cookie(w, "baseguard_session", token, 12*3600)
	s.cookie(w, "baseguard_login", "", -1)
	http.Redirect(w, r, "/", 303)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie("baseguard_session")
	_, err := s.Store.DB.Exec("DELETE FROM sessions WHERE token=?", tokenHash(c.Value))
	if err != nil {
		s.failure(w, err)
		return
	}
	s.cookie(w, "baseguard_session", "", -1)
	http.Redirect(w, r, "/login", 303)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ds, err := s.Store.Databases()
	if err != nil {
		s.failure(w, err)
		return
	}
	rs, err := s.Store.Runs()
	if err != nil {
		s.failure(w, err)
		return
	}
	d := Dashboard{Databases: ds, Runs: rs, CSRF: csrf(r), Flash: r.URL.Query().Get("message"), Total: len(ds)}
	for _, db := range ds {
		if db.Enabled {
			d.Active++
		}
	}
	for _, run := range rs {
		if run.Status == "success" {
			d.Success++
		}
		if run.Status == "failed" || run.Status == "interrupted" {
			d.Failed++
		}
	}
	t := "dashboard"
	if r.URL.Path == "/status" {
		t = "overview"
	}
	s.render(w, t, d)
}
func id(r *http.Request) int64 { n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64); return n }
func (s *Server) form(w http.ResponseWriter, r *http.Request) {
	d := DefaultDatabase()
	if r.PathValue("id") != "" {
		var err error
		d, err = s.Store.Database(id(r))
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}
	d.Password = ""
	s.render(w, "form", Dashboard{CSRF: csrf(r), Editing: d, Destinations: s.Worker.Destinations})
}
func number(r *http.Request, key string) int { n, _ := strconv.Atoi(r.FormValue(key)); return n }
func (s *Server) submitted(r *http.Request) (Database, error) {
	n, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	d := Database{ID: n, Name: strings.TrimSpace(r.FormValue("name")), Engine: r.FormValue("engine"), Host: strings.TrimSpace(r.FormValue("host")), DBName: r.FormValue("dbname"), Username: r.FormValue("username"), Password: r.FormValue("password"), TLSMode: r.FormValue("tls"), CAFile: r.FormValue("ca"), Port: number(r, "port"), Destination: r.FormValue("destination"), Subdir: r.FormValue("subdir"), ScheduleKind: r.FormValue("schedule"), At: r.FormValue("at"), Timezone: r.FormValue("timezone"), Interval: number(r, "interval"), Weekday: number(r, "weekday"), Retain: number(r, "retain"), Timeout: number(r, "timeout"), Enabled: r.FormValue("enabled") == "on"}
	if n != 0 {
		old, err := s.Store.Database(n)
		if err != nil {
			return d, err
		}
		if d.Password == "" {
			d.Password = old.Password
		}
		d.NextRun = old.NextRun
		if old.ScheduleKind != d.ScheduleKind || old.At != d.At || old.Timezone != d.Timezone || old.Interval != d.Interval || old.Weekday != d.Weekday || old.Enabled != d.Enabled {
			d.NextRun = 0
		}
	}
	if err := d.Validate(s.Worker.Destinations); err != nil {
		return d, err
	}
	if d.Enabled && d.NextRun == 0 {
		schedule, _ := d.Schedule()
		d.NextRun = schedule.Next(time.Now()).Unix()
	}
	if !d.Enabled {
		d.NextRun = 0
	}
	return d, nil
}
func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	d, err := s.submitted(r)
	if err != nil {
		d.Password = ""
		w.WriteHeader(422)
		s.render(w, "form", Dashboard{CSRF: csrf(r), Editing: d, Destinations: s.Worker.Destinations, Error: err.Error()})
		return
	}
	if err = s.Store.Save(&d); err != nil {
		s.failure(w, err)
		return
	}
	s.redirect(w, r, "Banco salvo. Você pode testar ou executar o primeiro backup.")
}
func (s *Server) test(w http.ResponseWriter, r *http.Request) {
	d, err := s.submitted(r)
	message := ""
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		message, err = s.Worker.Executor.Check(ctx, d)
	}
	data := Dashboard{Flash: message}
	if err != nil {
		data.Error = err.Error()
	}
	s.render(w, "message", data)
}
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.Database(id(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ok, err := s.Store.Enqueue(d, "manual", time.Now())
	if err != nil {
		s.failure(w, err)
		return
	}
	msg := "Backup adicionado à fila."
	if !ok {
		msg = "Este banco já tem um backup na fila ou em andamento."
	}
	s.redirect(w, r, msg)
}
func (s *Server) toggle(w http.ResponseWriter, r *http.Request) {
	d, err := s.Store.Database(id(r))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d.Enabled = !d.Enabled
	d.NextRun = 0
	if d.Enabled {
		schedule, e := d.Schedule()
		if e != nil {
			s.failure(w, e)
			return
		}
		d.NextRun = schedule.Next(time.Now()).Unix()
	}
	_, err = s.Store.DB.Exec("UPDATE databases SET enabled=?,next_run=? WHERE id=?", d.Enabled, d.NextRun, d.ID)
	if err != nil {
		s.failure(w, err)
		return
	}
	s.redirect(w, r, "Agendamento atualizado. Backups já enfileirados continuam normalmente.")
}
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	res, err := s.Store.DB.Exec("DELETE FROM databases WHERE id=? AND NOT EXISTS (SELECT 1 FROM runs WHERE database_id=? AND status IN ('queued','running'))", id(r), id(r))
	if err != nil {
		s.failure(w, err)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		s.redirect(w, r, "Não foi possível remover: banco inexistente ou backup pendente.")
		return
	}
	s.redirect(w, r, "Cadastro removido. Arquivos e histórico foram preservados.")
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.Run(id(r))
	if err != nil || run.Status != "success" {
		http.NotFound(w, r)
		return
	}
	root, err := openDestination(s.Worker.Destinations, run.Destination)
	if err != nil {
		http.Error(w, "Destino indisponível", 503)
		return
	}
	defer root.Close()
	f, err := root.Open(run.Path)
	if err != nil {
		http.Error(w, "Arquivo indisponível", 404)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "Arquivo inválido", 404)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(run.Path)))
	w.Header().Set("X-Checksum-SHA256", run.SHA256)
	http.ServeContent(w, r, filepath.Base(run.Path), info.ModTime(), f)
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings", Dashboard{CSRF: csrf(r), Destinations: s.Worker.Destinations})
}
func (s *Server) password(w http.ResponseWriter, r *http.Request) {
	h, err := s.Store.Setting("admin_hash")
	if err != nil {
		s.failure(w, err)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte(r.FormValue("current"))) != nil {
		http.Error(w, "Senha atual incorreta", 422)
		return
	}
	p := r.FormValue("password")
	if len(p) < 12 || len(p) > 72 {
		http.Error(w, "Use de 12 a 72 bytes na nova senha", 422)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if err != nil {
		s.failure(w, err)
		return
	}
	tx, err := s.Store.DB.Begin()
	if err != nil {
		s.failure(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE settings SET value=? WHERE key='admin_hash'", string(hash)); err == nil {
		_, err = tx.Exec("DELETE FROM sessions")
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.failure(w, err)
		return
	}
	s.cookie(w, "baseguard_session", "", -1)
	http.Redirect(w, r, "/login", 303)
}
