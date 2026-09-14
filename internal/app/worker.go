package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/robfig/cron/v3"
)

type Worker struct {
	Store        *Store
	Destinations map[string]string
	Executor     Executor
}

func (w *Worker) Recover() error {
	rows, err := w.Store.DB.Query("SELECT " + runColumns + " FROM runs WHERE status='running'")
	if err != nil {
		return err
	}
	var rs []Run
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			rows.Close()
			return e
		}
		rs = append(rs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Path != "" {
			root, e := openDestination(w.Destinations, r.Destination)
			if e == nil {
				_ = root.Remove(r.Path + ".partial")
				root.Close()
			}
		}
	}
	_, err = w.Store.DB.Exec("UPDATE runs SET status='interrupted',finished=?,error='Aplicação reiniciada durante o backup; arquivo não confirmado.' WHERE status='running'", time.Now().Unix())
	return err
}

func (w *Worker) Schedule(now time.Time, trigger string) error {
	dbs, err := w.Store.Databases()
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if !d.Enabled || d.NextRun > now.Unix() {
			continue
		}
		schedule, err := d.Schedule()
		if err != nil {
			return err
		}
		tx, err := w.Store.DB.Begin()
		if err != nil {
			return err
		}
		res, err := tx.Exec("UPDATE databases SET next_run=? WHERE id=? AND enabled=1 AND next_run=?", schedule.Next(now).Unix(), d.ID, d.NextRun)
		if err != nil {
			tx.Rollback()
			return err
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			_, err = tx.Exec("INSERT INTO runs(database_id,database_name,status,trigger,created) SELECT ?,?,'queued',?,? WHERE NOT EXISTS (SELECT 1 FROM runs WHERE database_id=? AND status IN ('queued','running'))", d.ID, d.Name, trigger, now.Unix(), d.ID)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c := cron.New()
		_, _ = c.AddFunc("* * * * *", func() {
			if err := w.Schedule(time.Now(), "scheduled"); err != nil {
				slog.Error("agendamento", "error", err)
			}
		})
		c.Start()
		defer func() { <-c.Stop().Done() }()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				r, err := w.Store.Claim()
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					slog.Error("fila", "error", err)
					continue
				}
				w.execute(ctx, r)
			}
		}
	}()
	return done
}

func (w *Worker) execute(parent context.Context, run Run) {
	d, err := w.Store.Database(run.DatabaseID)
	if err == nil {
		ctx, cancel := context.WithTimeout(parent, time.Duration(d.Timeout)*time.Minute)
		defer cancel()
		ex := w.Executor
		if native, ok := ex.(interface {
			Prepare(context.Context, Database) (preparedBackup, error)
		}); ok {
			var prepared preparedBackup
			prepared, err = native.Prepare(ctx, d)
			if err == nil {
				var info []byte
				info, err = json.Marshal(prepared.Info)
				if err == nil {
					_, err = w.Store.DB.Exec("UPDATE runs SET client_info=? WHERE id=?", string(info), run.ID)
				}
				ex = prepared
			}
		} else {
			_, err = ex.Check(ctx, d)
		}
		if err == nil {
			err = w.backupWithExecutor(ctx, d, run, ex)
		}
	}
	if err != nil {
		_, e := w.Store.DB.Exec("UPDATE runs SET status='failed',finished=?,error=? WHERE id=?", time.Now().Unix(), err.Error(), run.ID)
		slog.Error("backup falhou", "run", run.ID, "error", err)
		if e != nil {
			slog.Error("registro da falha", "error", e)
		}
	}
}

func (w *Worker) backup(ctx context.Context, d Database, run Run) error {
	return w.backupWithExecutor(ctx, d, run, w.Executor)
}

func (w *Worker) backupWithExecutor(ctx context.Context, d Database, run Run, ex Executor) error {
	r, err := openDestination(w.Destinations, d.Destination)
	if err != nil {
		return err
	}
	defer r.Close()
	sub := filepath.Join(d.Subdir, fmt.Sprintf("db-%d", d.ID))
	if err = r.MkdirAll(sub, 0700); err != nil {
		return err
	}
	ext := ".dump"
	if d.Engine != "postgres" {
		ext = ".sql.gz"
	}
	p := filepath.Join(sub, fmt.Sprintf("%s-%d%s", time.Unix(run.Created, 0).UTC().Format("20060102T150405Z"), run.ID, ext))
	_, err = w.Store.DB.Exec("UPDATE runs SET destination=?,path=? WHERE id=?", d.Destination, p, run.ID)
	if err != nil {
		return err
	}
	size, sum, err := writeBackup(ctx, r, p, d, ex)
	if err != nil {
		return err
	}
	_, err = w.Store.DB.Exec("UPDATE runs SET status='success',size=?,sha256=?,finished=? WHERE id=?", size, sum, time.Now().Unix(), run.ID)
	if err != nil {
		return err
	}
	if err = w.Retain(d); err != nil {
		_, _ = w.Store.DB.Exec("UPDATE runs SET error=? WHERE id=?", "Backup concluído; retenção pendente: "+err.Error(), run.ID)
		slog.Error("retenção", "error", err)
	}
	return nil
}

func (w *Worker) Retain(d Database) error {
	rows, err := w.Store.DB.Query("SELECT "+runColumns+" FROM runs WHERE database_id=? AND status='success' ORDER BY id DESC LIMIT -1 OFFSET ?", d.ID, d.Retain)
	if err != nil {
		return err
	}
	var rs []Run
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			rows.Close()
			return e
		}
		rs = append(rs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, run := range rs {
		r, err := openDestination(w.Destinations, run.Destination)
		if err != nil {
			return err
		}
		err = r.Remove(run.Path)
		r.Close()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err = w.Store.DB.Exec("UPDATE runs SET status='expired' WHERE id=?", run.ID); err != nil {
			return err
		}
	}
	return nil
}
