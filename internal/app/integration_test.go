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
	for _, engine := range []string{"postgres", "mysql"} {
		t.Run(engine, func(t *testing.T) {
			s, w, d := fixture(t)
			d.Engine = engine
			d.Host = engine
			d.DBName = "fixture"
			d.Username = "postgres"
			d.Password = "integration-only-password"
			d.TLSMode = "disable"
			if engine == "mysql" {
				d.Port = 3306
				d.Username = "root"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			sqlRun := func(db Database, query string) string {
				t.Helper()
				env, args, cleanup, err := credentials(db)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				var out bytes.Buffer
				client := "psql"
				if engine == "postgres" {
					args = append(args, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "--dbname="+db.DBName, "--command="+query)
				} else {
					client = "mysql"
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
			sqlRun(admin, "CREATE DATABASE fixture;")
			sqlRun(admin, "CREATE DATABASE restored;")
			ddl := `CREATE TABLE accounts (id INTEGER PRIMARY KEY, amount INTEGER NOT NULL); CREATE TABLE audit (account_id INTEGER); CREATE VIEW balances AS SELECT SUM(amount) AS total FROM accounts;`
			if engine == "postgres" {
				ddl += `CREATE FUNCTION record_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO audit VALUES (NEW.id); RETURN NEW; END $$; CREATE TRIGGER account_insert AFTER INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION record_insert(); CREATE FUNCTION doubled(x INTEGER) RETURNS INTEGER LANGUAGE SQL AS 'SELECT x * 2';`
			} else {
				ddl += `CREATE TRIGGER account_insert AFTER INSERT ON accounts FOR EACH ROW INSERT INTO audit VALUES (NEW.id); CREATE PROCEDURE count_accounts() SELECT COUNT(*) FROM accounts; CREATE EVENT refresh_marker ON SCHEDULE EVERY 1 DAY DISABLE DO INSERT INTO audit VALUES (999);`
			}
			sqlRun(d, ddl+`INSERT INTO accounts VALUES (1,120),(2,230);`)
			w.Executor = NativeExecutor{}
			if err := s.Save(&d); err != nil {
				t.Fatal(err)
			}
			bad := d
			bad.Password = "incorrect"
			if _, err := w.Executor.Check(ctx, bad); err == nil {
				t.Fatal("invalid credentials accepted")
			}
			if engine == "mysql" {
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
				if err = command(ctx, restored, "pg_restore", append(args, "--exit-on-error", "--dbname=restored", path), env, &out); err != nil {
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
				cmd := exec.CommandContext(ctx, "mysql", append(args, "--database=restored")...)
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
