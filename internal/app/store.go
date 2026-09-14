package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB  *sql.DB
	Key []byte
}

func OpenStore(path, keypath string) (*Store, error) {
	key, err := loadKey(keypath, path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Key: key}
	var schemaVersion int
	if err = db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil || schemaVersion > 2 {
		db.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("banco criado por versão mais recente do BaseGuard")
	}
	_, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS databases (id INTEGER PRIMARY KEY AUTOINCREMENT, config TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 0, next_run INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS runs (id INTEGER PRIMARY KEY AUTOINCREMENT,database_id INTEGER NOT NULL, database_name TEXT NOT NULL,status TEXT NOT NULL,trigger TEXT NOT NULL,destination TEXT NOT NULL DEFAULT '',path TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',sha256 TEXT NOT NULL DEFAULT '',created INTEGER NOT NULL,started INTEGER NOT NULL DEFAULT 0,finished INTEGER NOT NULL DEFAULT 0,size INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS one_active_run ON runs(database_id) WHERE status IN ('queued','running');
CREATE TABLE IF NOT EXISTS sessions (token TEXT PRIMARY KEY,csrf TEXT NOT NULL,expires INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if schemaVersion < 2 {
		tx, e := db.Begin()
		if e != nil {
			db.Close()
			return nil, e
		}
		if _, e = tx.Exec("ALTER TABLE runs ADD COLUMN client_info TEXT NOT NULL DEFAULT '{}'; PRAGMA user_version=2;"); e != nil {
			tx.Rollback()
			db.Close()
			return nil, e
		}
		if e = tx.Commit(); e != nil {
			db.Close()
			return nil, e
		}
	}
	_ = os.Chmod(path, 0600)
	return s, nil
}

func (s *Store) Save(d *Database) error {
	// Read the current schedule in the same transaction as the update so a
	// concurrent scheduler tick cannot be undone by an edit to the connection.
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if d.ID != 0 {
		var raw string
		var enabled bool
		var next int64
		if err = tx.QueryRow("SELECT config,enabled,next_run FROM databases WHERE id=?", d.ID).Scan(&raw, &enabled, &next); err != nil {
			return err
		}
		var old Database
		if err = json.Unmarshal([]byte(raw), &old); err != nil {
			return err
		}
		if enabled == d.Enabled && old.ScheduleKind == d.ScheduleKind && old.At == d.At && old.Timezone == d.Timezone && old.Interval == d.Interval && old.Weekday == d.Weekday {
			d.NextRun = next
		}
	}
	copy := *d
	copy.Password, err = encrypt(s.Key, d.Password)
	if err != nil {
		return err
	}
	b, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	if d.ID == 0 {
		r, e := tx.Exec("INSERT INTO databases(config,enabled,next_run) VALUES(?,?,?)", string(b), d.Enabled, d.NextRun)
		if e != nil {
			return e
		}
		d.ID, err = r.LastInsertId()
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.Exec("UPDATE databases SET config=?,enabled=?,next_run=? WHERE id=?", string(b), d.Enabled, d.NextRun, d.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Databases() ([]Database, error) {
	rows, err := s.DB.Query("SELECT id,config,enabled,next_run FROM databases ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Database
	for rows.Next() {
		var d Database
		var raw string
		var id, next int64
		var enabled bool
		if err = rows.Scan(&id, &raw, &enabled, &next); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		d.ID = id
		d.NextRun = next
		d.Enabled = enabled
		d.Password, err = decrypt(s.Key, d.Password)
		if err != nil {
			return nil, fmt.Errorf("não foi possível decifrar banco %d: %w", id, err)
		}
		list = append(list, d)
	}
	return list, rows.Err()
}
func (s *Store) Database(id int64) (Database, error) {
	ds, err := s.Databases()
	if err != nil {
		return Database{}, err
	}
	for _, d := range ds {
		if d.ID == id {
			return d, nil
		}
	}
	return Database{}, sql.ErrNoRows
}

func (s *Store) Enqueue(d Database, trigger string, now time.Time) (bool, error) {
	r, err := s.DB.Exec("INSERT INTO runs(database_id,database_name,status,trigger,created) SELECT ?,?,'queued',?,? WHERE EXISTS (SELECT 1 FROM databases WHERE id=?) AND NOT EXISTS (SELECT 1 FROM runs WHERE database_id=? AND status IN ('queued','running'))", d.ID, d.Name, trigger, now.Unix(), d.ID, d.ID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

const runColumns = "id,database_id,database_name,status,trigger,destination,path,error,sha256,created,started,finished,size,client_info"

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var info string
	err := row.Scan(&r.ID, &r.DatabaseID, &r.DatabaseName, &r.Status, &r.Trigger, &r.Destination, &r.Path, &r.Error, &r.SHA256, &r.Created, &r.Started, &r.Finished, &r.Size, &info)
	if err == nil {
		err = json.Unmarshal([]byte(info), &r.ClientInfo)
	}
	return r, err
}
func (s *Store) Runs() ([]Run, error) {
	rows, err := s.DB.Query("SELECT " + runColumns + " FROM runs ORDER BY id DESC LIMIT 200")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rs []Run
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			return nil, e
		}
		rs = append(rs, r)
	}
	return rs, rows.Err()
}
func (s *Store) Run(id int64) (Run, error) {
	return scanRun(s.DB.QueryRow("SELECT "+runColumns+" FROM runs WHERE id=?", id))
}
func (s *Store) Claim() (Run, error) {
	return scanRun(s.DB.QueryRow("UPDATE runs SET status='running',started=? WHERE id=(SELECT id FROM runs WHERE status='queued' ORDER BY id LIMIT 1) RETURNING "+runColumns, time.Now().Unix()))
}
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.DB.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	return v, err
}
