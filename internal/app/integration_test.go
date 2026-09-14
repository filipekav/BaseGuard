package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This suite uses only the disposable services from compose.test.yaml.
// No credentials, host names or production databases are taken from the app.
func TestIntegrationRestore(t *testing.T) {
	if os.Getenv("BASEGUARD_INTEGRATION") != "1" {
		t.Skip("run with compose.test.yaml")
	}
	for _, engine := range []string{os.Getenv("BASEGUARD_TEST_ENGINE")} {
		t.Run(engine, func(t *testing.T) {
			s, w, d := fixture(t)
			d.Engine = engine
			d.Host = "db"
			d.DBName = "fixture"
			d.Username = "postgres"
			d.Password = "integration-only-password"
			d.TLSMode = "disable"
			if engine != "postgres" {
				d.Port = 3306
				d.Username = "root"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			profile, e := profileFor(engine, os.Getenv("BASEGUARD_TEST_SERIES"))
			if e != nil {
				t.Fatal(e)
			}
			defer cancel()
			sqlRun := func(db Database, query string) string {
				t.Helper()
				env, args, cleanup, err := credentials(db)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				var out bytes.Buffer
				client := profile.binary(profile.SQL)
				if engine == "postgres" {
					args = append(args, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "--dbname="+db.DBName, "--command="+query)
				} else {
					client = profile.binary(profile.SQL)
					args = append(args, "--batch", "--skip-column-names", "--database="+db.DBName, "--execute="+query)
				}
				if err = command(ctx, db, client, args, env, &out); err != nil {
					t.Fatal(err)
				}
				return strings.TrimSpace(out.String())
			}
			admin := d
			if engine == "postgres" {
				admin.DBName = "postgres"
			} else {
				admin.DBName = "mysql"
			}
			// Readiness is checked using the same native client used by the application.
			ready := false
			for attempt := 0; attempt < 90; attempt++ {
				env, args, cleanup, e := credentials(admin)
				if e != nil {
					t.Fatal(e)
				}
				if engine == "postgres" {
					args = append(args, "-X", "--dbname="+admin.DBName, "--command=SELECT 1")
				} else {
					args = append(args, "--connect-timeout=2", "--database="+admin.DBName, "--execute=SELECT 1")
				}
				e = command(ctx, admin, profile.binary(profile.SQL), args, env, io.Discard)
				cleanup()
				if e == nil {
					ready = true
					break
				}
				time.Sleep(2 * time.Second)
			}
			if !ready {
				t.Fatal("database did not become ready")
			}
			createDB := "CREATE DATABASE fixture; CREATE DATABASE restored;"
			if engine != "postgres" {
				createDB = "CREATE DATABASE fixture CHARACTER SET utf8mb4; CREATE DATABASE restored CHARACTER SET utf8mb4;"
			}
			sqlRun(admin, createDB)
			ddl := `CREATE TABLE accounts (id INTEGER PRIMARY KEY, amount INTEGER NOT NULL); CREATE TABLE audit (account_id INTEGER); CREATE VIEW balances AS SELECT SUM(amount) AS total FROM accounts;`
			if engine == "postgres" {
				ddl += `CREATE FUNCTION record_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO audit VALUES (NEW.id); RETURN NEW; END $$; CREATE TRIGGER account_insert AFTER INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION record_insert(); CREATE FUNCTION doubled(x INTEGER) RETURNS INTEGER LANGUAGE SQL AS 'SELECT x * 2';`
			} else {
				ddl += `CREATE TRIGGER account_insert AFTER INSERT ON accounts FOR EACH ROW INSERT INTO audit VALUES (NEW.id); CREATE PROCEDURE count_accounts() SELECT COUNT(*) FROM accounts; CREATE EVENT refresh_marker ON SCHEDULE EVERY 1 DAY DISABLE DO INSERT INTO audit VALUES (999);`
			}
			sqlRun(d, ddl+`INSERT INTO accounts VALUES (1,120),(2,230); CREATE TABLE notes (id INTEGER PRIMARY KEY, account_id INTEGER, note VARCHAR(100), optional_value INTEGER, FOREIGN KEY (account_id) REFERENCES accounts(id)); CREATE INDEX notes_account ON notes(account_id); INSERT INTO notes VALUES (1,1,'ação 日本語',NULL);`)
			binaryDDL := "CREATE TABLE payloads (id INTEGER PRIMARY KEY, payload BYTEA); INSERT INTO payloads VALUES (1,decode('00ff1027','hex'));"
			if engine != "postgres" {
				binaryDDL = "CREATE TABLE payloads (id INTEGER PRIMARY KEY, payload BLOB); INSERT INTO payloads VALUES (1,UNHEX('00ff1027'));"
			}
			sqlRun(d, binaryDDL)
			w.Executor = NativeExecutor{}
			if os.Getenv("BASEGUARD_TEST_NO_TLS") == "1" {
				if _, err := w.Executor.Check(ctx, d); err != nil {
					t.Fatalf("explicit plaintext connection: %v", err)
				}
				d.TLSMode = "require"
				if _, err := w.Executor.Check(ctx, d); err == nil {
					t.Fatal("TLS required silently downgraded to plaintext")
				}
				if err := w.Executor.Dump(ctx, d, io.Discard); err == nil {
					t.Fatal("dump silently downgraded to plaintext")
				}
				return
			}
			limited := d
			limited.Username = "limited"
			limited.Password = "integration-limited"
			if engine == "postgres" {
				sqlRun(admin, "CREATE ROLE limited LOGIN PASSWORD 'integration-limited';")
				if err := w.Executor.Dump(ctx, limited, io.Discard); err == nil {
					t.Fatal("dump accepted without table privileges")
				}
			} else {
				sqlRun(admin, "CREATE USER 'limited'@'%' IDENTIFIED BY 'integration-limited';")
				if _, err := w.Executor.Check(ctx, limited); err == nil {
					t.Fatal("database access accepted without grants")
				}
			}
			for _, mode := range []string{"require", "verify-full"} {
				secure := d
				secure.TLSMode = mode
				secure.CAFile = "/certs/ca.crt"
				if _, err := w.Executor.Check(ctx, secure); err != nil {
					t.Fatalf("TLS %s: %v", mode, err)
				}
				if err := w.Executor.Dump(ctx, secure, io.Discard); err != nil {
					t.Fatalf("TLS dump %s: %v", mode, err)
				}
			}
			wrongCA := d
			wrongCA.TLSMode = "verify-full"
			wrongCA.CAFile = "/certs/wrong.crt"
			if _, err := w.Executor.Check(ctx, wrongCA); err == nil {
				t.Fatal("invalid CA accepted")
			}
			wrongHost := wrongCA
			wrongHost.CAFile = "/certs/ca.crt"
			wrongHost.Host = "wronghost"
			if _, err := w.Executor.Check(ctx, wrongHost); err == nil {
				t.Fatal("invalid certificate hostname accepted")
			}
			// Exercise the worker and restore with certificate verification too.
			d.TLSMode = "verify-full"
			d.CAFile = "/certs/ca.crt"
			if err := s.Save(&d); err != nil {
				t.Fatal(err)
			}
			bad := d
			bad.Password = "incorrect"
			if _, err := w.Executor.Check(ctx, bad); err == nil {
				t.Fatal("invalid credentials accepted")
			}
			if engine != "postgres" {
				sqlRun(d, "CREATE TABLE legacy (id INT) ENGINE=MyISAM;")
				if _, err := w.Executor.Check(ctx, d); err == nil {
					t.Fatal("non-InnoDB accepted")
				}
				sqlRun(d, "DROP TABLE legacy;")
			}
			start := time.Now()
			_, err := s.Enqueue(d, "manual", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.Claim()
			if err != nil {
				t.Fatal(err)
			}
			w.execute(ctx, run)
			run, _ = s.Run(run.ID)
			if run.ClientInfo.Profile != profile.id() || run.ClientInfo.ClientVersion == "" {
				t.Fatalf("missing/wrong client metadata: %+v", run.ClientInfo)
			}
			if run.Status != "success" {
				t.Fatalf("backup: %+v", run)
			}
			restored := d
			restored.DBName = "restored"
			env, args, cleanup, err := credentials(restored)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			path := filepath.Join(w.Destinations[d.Destination], run.Path)
			if engine == "postgres" {
				var out bytes.Buffer
				if err = command(ctx, restored, profile.binary(profile.Restore), append(args, "--exit-on-error", "--dbname=restored", path), env, &out); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				gz, err := gzip.NewReader(f)
				if err != nil {
					t.Fatal(err)
				}
				defer gz.Close()
				cmd := exec.CommandContext(ctx, profile.binary(profile.Restore), append(args, "--database=restored")...)
				cmd.Env = env
				cmd.Stdin = gz
				var output bytes.Buffer
				cmd.Stdout = &output
				cmd.Stderr = &output
				if err = cmd.Run(); err != nil {
					t.Fatalf("restore: %v: %s", err, output.String())
				}
				if _, err = io.Copy(io.Discard, gz); err != nil {
					t.Fatal(err)
				}
			}
			if got := sqlRun(restored, "SELECT total FROM balances;"); got != "350" {
				t.Fatalf("restored data/view: %s", got)
			}
			if sqlRun(restored, "SELECT note FROM notes WHERE optional_value IS NULL;") != "ação 日本語" {
				t.Fatal("unicode/null data not restored")
			}
			binaryQuery := "SELECT encode(payload,'hex') FROM payloads;"
			if engine != "postgres" {
				binaryQuery = "SELECT LOWER(HEX(payload)) FROM payloads;"
			}
			if sqlRun(restored, binaryQuery) != "00ff1027" {
				t.Fatal("binary data not restored")
			}
			fkQuery := "SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_name='notes' AND constraint_type='FOREIGN KEY' AND table_schema='public';"
			indexQuery := "SELECT COUNT(*) FROM pg_indexes WHERE tablename='notes' AND indexname='notes_account';"
			if engine != "postgres" {
				fkQuery = "SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_name='notes' AND constraint_type='FOREIGN KEY' AND table_schema='restored';"
				indexQuery = "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema='restored' AND table_name='notes' AND index_name='notes_account';"
			}
			if sqlRun(restored, fkQuery) != "1" || sqlRun(restored, indexQuery) != "1" {
				t.Fatal("foreign key/index not restored")
			}
			sqlRun(restored, "INSERT INTO accounts VALUES (3,50);")
			if got := sqlRun(restored, "SELECT COUNT(*) FROM audit;"); got != "3" {
				t.Fatalf("restored trigger: %s", got)
			}
			if engine == "postgres" {
				if sqlRun(restored, "SELECT doubled(7);") != "14" {
					t.Fatal("function not restored")
				}
			} else {
				if sqlRun(restored, "CALL count_accounts();") != "3" {
					t.Fatal("routine not restored")
				}
				if sqlRun(restored, "SELECT COUNT(*) FROM information_schema.events WHERE event_schema='restored' AND event_name='refresh_marker';") != "1" {
					t.Fatal("event not restored")
				}
			}
			t.Logf("%s: dump %d bytes, backup + restore %s", engine, run.Size, time.Since(start))
		})
	}
}
