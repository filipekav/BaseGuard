package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerProfileSelection(t *testing.T) {
	for _, tc := range []struct{ engine, raw, vendor, series, client string }{
		{"mysql", "5.7.42-0ubuntu0.18.04.1", "Ubuntu", "5.7", "8.0"},
		{"mysql", "5.7.44", "MySQL Community Server (GPL)", "5.7", "8.0"},
		{"mysql", "8.0.46", "MySQL Community Server", "8.0", "8.0"},
		{"mysql", "8.4.11", "MySQL Community Server", "8.4", "8.4"},
		{"mariadb", "5.5.5-10.6.28-MariaDB", "MariaDB Server", "10.6", "10.6"},
		{"mariadb", "10.11.19-MariaDB-ubu2204", "MariaDB Server", "10.11", "10.11"},
		{"mariadb", "11.4.13-MariaDB", "MariaDB", "11.4", "11.4"},
		{"mariadb", "11.8.9-MariaDB", "MariaDB", "11.8", "11.8"},
		{"mariadb", "12.3.3-MariaDB", "MariaDB", "12.3", "12.3"},
		{"postgres", "12.22 (Debian)", "", "12", "12"},
		{"postgres", "18.6", "", "18", "18"},
	} {
		t.Run(tc.engine+tc.raw, func(t *testing.T) {
			series, err := serverSeries(tc.engine, tc.raw, tc.vendor)
			if err != nil || series != tc.series {
				t.Fatalf("series=%s err=%v", series, err)
			}
			p, err := profileFor(tc.engine, series)
			if err != nil || p.ClientSeries != tc.client {
				t.Fatalf("profile=%+v err=%v", p, err)
			}
			root := t.TempDir()
			t.Setenv("BASEGUARD_CLIENTS_DIR", root)
			path := p.binary(p.Dump)
			if !filepath.IsAbs(path) || !strings.HasPrefix(path, root+string(filepath.Separator)) {
				t.Fatal(path)
			}
		})
	}
	for _, tc := range []struct{ engine, series string }{{"mysql", "5.6"}, {"mysql", "9.0"}, {"postgres", "11"}, {"postgres", "19"}, {"mariadb", "10.5"}, {"mariadb", "11.5"}, {"mariadb", "../../tmp"}} {
		if _, err := profileFor(tc.engine, tc.series); err == nil {
			t.Fatalf("unsupported profile accepted: %+v", tc)
		}
	}
	for _, engine := range []string{"mysql", "mariadb"} {
		raw := "8.0.46"
		if engine == "mysql" {
			raw = "10.11.19-MariaDB"
		}
		if _, err := serverSeries(engine, raw, ""); err == nil {
			t.Fatal("wrong family accepted")
		}
	}
}

func TestClientVersionParsers(t *testing.T) {
	for raw, want := range map[string]string{
		"pg_dump (PostgreSQL) 12.22 (Debian 12.22-1)":                               "12.22",
		"psql (PostgreSQL) 18.6":                                                    "18.6",
		"mysqldump  Ver 8.0.46 for Linux on aarch64 (MySQL Community Server - GPL)": "8.0.46",
		"mariadb-dump  Ver 10.19 Distrib 10.11.19-MariaDB, for debian-linux-gnu":    "10.11.19",
		"mysql  Ver 14.14 Distrib 5.7.42, for Linux":                                "5.7.42",
		"mariadb from 12.3.3-MariaDB, client 15.2 for Linux":                        "12.3.3",
	} {
		got, err := clientVersion(raw)
		if err != nil || got != want {
			t.Fatalf("%s => %s (%v)", raw, got, err)
		}
	}
	if _, err := clientVersion("unknown client 1"); err == nil {
		t.Fatal("unknown accepted")
	}
}

func TestTLSArgumentsByFamily(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql", "mariadb"} {
		for _, mode := range []string{"disable", "require", "verify-full"} {
			d := DefaultDatabase()
			d.Engine = engine
			d.TLSMode = mode
			d.Host = "db"
			d.DBName = "fixture"
			d.Username = "backup"
			d.Password = "do-not-leak-this"
			env, args, cleanup, err := credentials(d)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "--get-server-public-key") != (engine == "mysql" && mode == "disable") {
				t.Fatalf("RSA key retrieval must be limited to MySQL with TLS explicitly disabled: %s", joined)
			}
			if strings.Contains(joined, d.Password) {
				t.Fatal("credential leaked")
			}
			if engine == "mariadb" {
				if strings.Contains(joined, "--ssl-mode") {
					t.Fatal("Oracle TLS flag used for MariaDB")
				}
				want := map[string]string{"disable": "--skip-ssl", "require": "--disable-ssl-verify-server-cert", "verify-full": "--ssl-verify-server-cert"}[mode]
				if !strings.Contains(joined, want) {
					t.Fatal(joined)
				}
			}
			if engine == "mysql" && !strings.Contains(joined, "--ssl-mode=") {
				t.Fatal(joined)
			}
			if engine != "postgres" && !strings.HasPrefix(args[0], "--defaults-file=") {
				t.Fatal("credentials file must be first")
			}
			if engine == "postgres" && mode == "verify-full" && !strings.Contains(strings.Join(env, "\n"), "PGSSLROOTCERT=/etc/ssl/certs/ca-certificates.crt") {
				t.Fatal("old libpq CA compatibility")
			}
			cleanup()
		}
	}
}

func TestMissingClientHasActionableError(t *testing.T) {
	t.Setenv("BASEGUARD_CLIENTS_DIR", t.TempDir())
	d := DefaultDatabase()
	d.Host = "unused"
	d.DBName = "app"
	d.Username = "backup"
	_, err := (NativeExecutor{}).Check(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "não instalado") {
		t.Fatal(err)
	}
}

func TestMigrateVersionOnePreservesConfigurationAndHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseguard.db")
	keypath := filepath.Join(dir, "master.key")
	s, err := OpenStore(path, keypath)
	if err != nil {
		t.Fatal(err)
	}
	d := DefaultDatabase()
	d.Name = "saved"
	d.Password = "original-password"
	if err = s.Save(&d); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue(d, "manual", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("ALTER TABLE runs DROP COLUMN client_info; PRAGMA user_version=1;"); err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(keypath)
	if err != nil {
		t.Fatal(err)
	}
	s.DB.Close()
	s, err = OpenStore(path, keypath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	saved, err := s.Database(d.ID)
	if err != nil || saved.Password != d.Password {
		t.Fatalf("config: %+v %v", saved, err)
	}
	runs, err := s.Runs()
	if err != nil || len(runs) != 1 || runs[0].ClientInfo.Summary() != "Versão não registrada" {
		t.Fatalf("history: %+v %v", runs, err)
	}
	info := ConnectionInfo{Engine: "mysql", ServerVersion: "5.7.42", ClientVersion: "8.0.46", Profile: "mysql-5.7", Legacy: true}
	raw, _ := json.Marshal(info)
	if _, err = s.DB.Exec("UPDATE runs SET client_info=?", string(raw)); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run(runs[0].ID)
	if err != nil || r.ClientInfo != info {
		t.Fatalf("metadata: %+v %v", r, err)
	}
	keyAfter, err := os.ReadFile(keypath)
	if err != nil || string(keyBefore) != string(keyAfter) {
		t.Fatal("master key changed")
	}
	var version int
	s.DB.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 2 {
		t.Fatal(version)
	}
}
