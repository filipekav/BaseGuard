package app

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

type Database struct {
	ID                                                              int64
	Name, Engine, Host, DBName, Username, Password, TLSMode, CAFile string
	Port                                                            int
	Destination, Subdir, ScheduleKind, At, Timezone                 string
	Interval, Weekday, Retain, Timeout                              int
	Enabled                                                         bool
	NextRun                                                         int64
}

type Run struct {
	ID, DatabaseID                                                  int64
	DatabaseName, Status, Trigger, Destination, Path, Error, SHA256 string
	Created, Started, Finished, Size                                int64
}

type Dashboard struct {
	Databases                      []Database
	Runs                           []Run
	Destinations                   map[string]string
	CSRF, Flash, Error             string
	Editing                        Database
	Total, Active, Success, Failed int
}

func DefaultDatabase() Database {
	return Database{Engine: "postgres", Port: 5432, TLSMode: "require", ScheduleKind: "daily", At: "02:00", Timezone: "America/Sao_Paulo", Interval: 24, Weekday: 0, Retain: 7, Timeout: 120}
}

func (d Database) Schedule() (cron.Schedule, error) {
	if _, err := time.LoadLocation(d.Timezone); err != nil {
		return nil, fmt.Errorf("fuso horário inválido")
	}
	var spec string
	switch d.ScheduleKind {
	case "interval":
		if d.Interval < 1 || d.Interval > 8760 {
			return nil, fmt.Errorf("intervalo deve ser de 1 a 8760 horas")
		}
		return cron.Every(time.Duration(d.Interval) * time.Hour), nil
	case "daily", "weekly":
		t, err := time.Parse("15:04", d.At)
		if err != nil {
			return nil, fmt.Errorf("horário inválido")
		}
		day := "*"
		if d.ScheduleKind == "weekly" {
			if d.Weekday < 0 || d.Weekday > 6 {
				return nil, fmt.Errorf("dia da semana inválido")
			}
			day = strconv.Itoa(d.Weekday)
		}
		spec = fmt.Sprintf("CRON_TZ=%s %d %d * * %s", d.Timezone, t.Minute(), t.Hour(), day)
	default:
		return nil, fmt.Errorf("frequência inválida")
	}
	return cron.ParseStandard(spec)
}

func (d Database) Validate(destinations map[string]string) error {
	if strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.Host) == "" || d.DBName == "" || d.Username == "" {
		return fmt.Errorf("preencha nome, host, banco e usuário")
	}
	for _, s := range []string{d.Name, d.Host, d.DBName, d.Username, d.Password, d.CAFile, d.Subdir} {
		if len(s) > 1024 || strings.ContainsAny(s, "\x00\r\n") {
			return fmt.Errorf("campo inválido ou muito longo")
		}
	}
	if strings.HasPrefix(d.Host, "-") || strings.HasPrefix(d.DBName, "-") || strings.ContainsAny(d.Host, "/\\=") {
		return fmt.Errorf("informe um host TCP e um nome de banco válido")
	}
	if strings.Contains(d.DBName, "=") || strings.HasPrefix(d.DBName, "postgres://") || strings.HasPrefix(d.DBName, "postgresql://") {
		return fmt.Errorf("informe apenas o nome do banco, sem string de conexão")
	}
	if d.Engine != "postgres" && d.Engine != "mysql" {
		return fmt.Errorf("mecanismo inválido")
	}
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("porta inválida")
	}
	if d.TLSMode != "disable" && d.TLSMode != "require" && d.TLSMode != "verify-full" {
		return fmt.Errorf("modo TLS inválido")
	}
	if d.CAFile != "" && !filepath.IsAbs(d.CAFile) {
		return fmt.Errorf("certificado CA deve usar caminho absoluto no container")
	}
	if _, ok := destinations[d.Destination]; !ok {
		return fmt.Errorf("destino não configurado")
	}
	if d.Subdir != "" && (!filepath.IsLocal(d.Subdir) || strings.ContainsAny(d.Subdir, "\\:")) {
		return fmt.Errorf("subpasta deve ser relativa ao destino")
	}
	if d.Retain < 1 || d.Retain > 1000 || d.Timeout < 1 || d.Timeout > 10080 {
		return fmt.Errorf("retenção deve ser de 1 a 1000; timeout de 1 a 10080 minutos")
	}
	_, err := d.Schedule()
	return err
}

func displayTime(t int64) string {
	if t == 0 {
		return "—"
	}
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	return time.Unix(t, 0).In(loc).Format("02/01 15:04")
}
