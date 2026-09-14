package app

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ConnectionInfo is persisted per run. It contains no connection credentials.
type ConnectionInfo struct {
	Engine        string `json:"engine"`
	ServerVersion string `json:"server_version"`
	ClientVersion string `json:"client_version"`
	Profile       string `json:"profile"`
	Legacy        bool   `json:"legacy"`
}

func (i ConnectionInfo) Summary() string {
	if i.Profile == "" {
		return "Versão não registrada"
	}
	s := fmt.Sprintf("%s %s · cliente %s · perfil %s", i.Engine, i.ServerVersion, i.ClientVersion, i.Profile)
	if i.Legacy {
		s += " · série legada; homologação não implica manutenção do fabricante"
	}
	return s
}

type clientProfile struct {
	Engine, Series, ClientSeries, SQL, Dump, Restore string
	Legacy                                           bool
}

func profileFor(engine, series string) (clientProfile, error) {
	p := clientProfile{Engine: engine, Series: series, ClientSeries: series}
	switch engine {
	case "postgres":
		n, err := strconv.Atoi(series)
		if err == nil && n >= 12 && n <= 18 {
			p.SQL, p.Dump, p.Restore, p.Legacy = "psql", "pg_dump", "pg_restore", n < 14
			return p, nil
		}
	case "mysql":
		if series == "5.7" || series == "8.0" || series == "8.4" {
			p.SQL, p.Dump, p.Restore = "mysql", "mysqldump", "mysql"
			if series == "5.7" {
				p.ClientSeries, p.Legacy = "8.0", true
			}
			return p, nil
		}
	case "mariadb":
		switch series {
		case "10.6", "10.11", "11.4", "11.8", "12.3":
			p.SQL, p.Dump, p.Restore, p.Legacy = "mariadb", "mariadb-dump", "mariadb", series == "10.6"
			return p, nil
		}
	}
	return p, fmt.Errorf("série %s %s fora da matriz: PostgreSQL 12–18; MySQL 5.7/8.0/8.4; MariaDB 10.6/10.11/11.4/11.8/12.3", engine, series)
}

func (p clientProfile) id() string { return p.Engine + "-" + p.Series }
func (p clientProfile) binary(name string) string {
	root := os.Getenv("BASEGUARD_CLIENTS_DIR")
	if root == "" {
		root = "/opt/baseguard/clients"
	}
	return filepath.Join(root, p.Engine+"-"+p.ClientSeries, "bin", name)
}

var numericVersion = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)
var distributionVersion = regexp.MustCompile(`(?i)(?:Distrib\s+|\(PostgreSQL\)\s+|\bVer\s+|\bfrom\s+)([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)

func serverSeries(engine, raw, vendor string) (string, error) {
	isMaria := strings.Contains(strings.ToLower(raw+" "+vendor), "mariadb")
	if (engine == "mysql" && isMaria) || (engine == "mariadb" && !isMaria) {
		return "", fmt.Errorf("mecanismo selecionado (%s) difere do servidor detectado; confira o campo Mecanismo", engine)
	}
	if isMaria {
		raw = strings.TrimPrefix(raw, "5.5.5-")
	}
	m := numericVersion.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", fmt.Errorf("versão de servidor não reconhecida: %s", raw)
	}
	if engine == "postgres" {
		return m[1], nil
	}
	return m[1] + "." + m[2], nil
}

func clientVersion(raw string) (string, error) {
	// MariaDB/MySQL legacy output can start with a utility version (10.13)
	// followed by the actual distribution version. Prefer Distrib explicitly.
	if i := strings.Index(strings.ToLower(raw), "distrib "); i >= 0 {
		raw = raw[i:]
	}
	m := distributionVersion.FindStringSubmatch(raw)
	if m == nil {
		return "", fmt.Errorf("versão de cliente não reconhecida: %s", strings.TrimSpace(raw))
	}
	return m[1], nil
}

type preparedBackup struct {
	Info    ConnectionInfo
	profile clientProfile
}

func (p preparedBackup) Check(context.Context, Database) (string, error) {
	return "Conexão OK · " + p.Info.Summary(), nil
}
func (p preparedBackup) Dump(ctx context.Context, d Database, w io.Writer) error {
	return dumpWithProfile(ctx, d, p.profile, w)
}

func (NativeExecutor) Prepare(ctx context.Context, d Database) (preparedBackup, error) {
	var prepared preparedBackup
	probeSeries := map[string]string{"postgres": "18", "mysql": "8.0", "mariadb": "12.3"}[d.Engine]
	probe, err := profileFor(d.Engine, probeSeries)
	if err != nil {
		return prepared, err
	}
	env, args, cleanup, err := credentials(d)
	if err != nil {
		return prepared, err
	}
	defer cleanup()
	var out limitedBuffer
	query := "SELECT VERSION(); SELECT @@version_comment; SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=CONVERT(0x" + hex.EncodeToString([]byte(d.DBName)) + " USING utf8mb4) AND TABLE_TYPE='BASE TABLE' AND (ENGINE IS NULL OR ENGINE <> 'InnoDB'); SHOW SESSION STATUS LIKE 'Ssl_cipher';"
	if d.Engine == "postgres" {
		args = append(args, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "--dbname="+d.DBName, "--command=SHOW server_version_num; SHOW server_version;")
	} else {
		args = append(args, "--connect-timeout=15", "--batch", "--skip-column-names", "--database="+d.DBName, "--execute="+query)
	}
	if err = command(ctx, d, probe.binary(probe.SQL), args, env, &out); err != nil {
		return prepared, err
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var raw, series string
	if d.Engine == "postgres" {
		if len(lines) != 2 {
			return prepared, fmt.Errorf("resposta inválida ao identificar PostgreSQL")
		}
		n, e := strconv.Atoi(strings.TrimSpace(lines[0]))
		if e != nil || n < 100000 {
			return prepared, fmt.Errorf("PostgreSQL anterior a 12 ou versão inválida")
		}
		series, raw = strconv.Itoa(n/10000), strings.TrimSpace(lines[1])
	} else {
		if len(lines) != 4 {
			return prepared, fmt.Errorf("resposta inválida ao identificar servidor e mecanismos")
		}
		raw = strings.TrimSpace(lines[0])
		series, err = serverSeries(d.Engine, raw, lines[1])
		if err != nil {
			return prepared, err
		}
		if strings.TrimSpace(lines[2]) != "0" {
			return prepared, fmt.Errorf("banco contém tabelas fora do InnoDB; backup consistente recusado")
		}
		if d.TLSMode != "disable" && len(strings.Fields(lines[3])) != 2 {
			return prepared, fmt.Errorf("servidor não estabeleceu TLS; conexão sem criptografia recusada")
		}
	}
	p, err := profileFor(d.Engine, series)
	if err != nil {
		return prepared, err
	}
	var versionOut limitedBuffer
	for _, name := range []string{p.SQL, p.Dump, p.Restore} {
		versionOut.Reset()
		if err = command(ctx, d, p.binary(name), []string{"--version"}, env, &versionOut); err != nil {
			return prepared, err
		}
		v, e := clientVersion(versionOut.String())
		if e != nil {
			return prepared, e
		}
		m := numericVersion.FindStringSubmatch(v)
		actual := m[1] + "." + m[2]
		if d.Engine == "postgres" {
			actual = m[1]
		}
		if actual != p.ClientSeries {
			return prepared, fmt.Errorf("cliente %s possui versão %s; perfil exige %s", name, v, p.ClientSeries)
		}
		if name == p.Dump {
			prepared.Info.ClientVersion = v
		}
	}
	prepared.profile = p
	prepared.Info.Engine, prepared.Info.ServerVersion, prepared.Info.Profile, prepared.Info.Legacy = d.Engine, raw, p.id(), p.Legacy
	return prepared, nil
}

func (n NativeExecutor) Check(ctx context.Context, d Database) (string, error) {
	p, err := n.Prepare(ctx, d)
	if err != nil {
		return "", err
	}
	return p.Check(ctx, d)
}
func (n NativeExecutor) Dump(ctx context.Context, d Database, w io.Writer) error {
	p, err := n.Prepare(ctx, d)
	if err != nil {
		return err
	}
	return p.Dump(ctx, d, w)
}
