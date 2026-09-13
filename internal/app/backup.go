package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Executor interface {
	Check(context.Context, Database) (string, error)
	Dump(context.Context, Database, io.Writer) error
}
type NativeExecutor struct{}
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 4096 {
		keep := min(len(p), 4096-b.Len())
		_, _ = b.Buffer.Write(p[:keep])
	}
	return n, nil
}

func credentials(d Database) (env []string, options []string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "baseguard-auth-*")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	if err = f.Chmod(0600); err != nil {
		f.Close()
		cleanup()
		return nil, nil, nil, err
	}
	env = os.Environ()
	var content string
	if d.Engine == "postgres" {
		esc := func(s string) string { return strings.NewReplacer("\\", "\\\\", ":", "\\:").Replace(s) }
		content = fmt.Sprintf("%s:%d:%s:%s:%s\n", esc(d.Host), d.Port, esc(d.DBName), esc(d.Username), esc(d.Password))
		env = append(env, "PGPASSFILE="+f.Name(), "PGSSLMODE="+d.TLSMode, "PGCONNECT_TIMEOUT=15")
		if d.CAFile != "" {
			env = append(env, "PGSSLROOTCERT="+d.CAFile)
		} else if d.TLSMode == "verify-full" {
			env = append(env, "PGSSLROOTCERT=system")
		}
		options = []string{"--host=" + d.Host, "--port=" + strconv.Itoa(d.Port), "--username=" + d.Username, "--no-password"}
	} else {
		esc := func(s string) string { return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s) }
		content = "[client]\npassword=\"" + esc(d.Password) + "\"\n"
		tls := map[string]string{"disable": "DISABLED", "require": "REQUIRED", "verify-full": "VERIFY_IDENTITY"}[d.TLSMode]
		options = []string{"--defaults-file=" + f.Name(), "--protocol=TCP", "--host=" + d.Host, "--port=" + strconv.Itoa(d.Port), "--user=" + d.Username, "--ssl-mode=" + tls, "--connect-timeout=15"}
		if d.CAFile != "" {
			options = append(options, "--ssl-ca="+d.CAFile)
		} else if d.TLSMode == "verify-full" {
			options = append(options, "--ssl-ca=/etc/ssl/certs/ca-certificates.crt")
		}
	}
	_, err = f.WriteString(content)
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		cleanup()
	}
	return
}

func command(ctx context.Context, d Database, name string, args []string, env []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = env
	cmd.Stdout = out
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("cliente %s não instalado; confira a imagem Docker e as versões suportadas", name)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("operação interrompida ou tempo máximo excedido")
		}
		message := stderr.String()
		if d.Password != "" {
			message = strings.ReplaceAll(message, d.Password, "[oculto]")
		}
		return fmt.Errorf("%s falhou: %v — %s", name, err, strings.TrimSpace(message))
	}
	return nil
}

var versionRE = regexp.MustCompile(`([0-9]+)\.([0-9]+)`)

func version(s string) (int, int, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("versão não reconhecida: %s", s)
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a, b, nil
}

func (NativeExecutor) Check(ctx context.Context, d Database) (string, error) {
	env, options, cleanup, err := credentials(d)
	if err != nil {
		return "", err
	}
	defer cleanup()
	var out limitedBuffer
	client := "pg_dump"
	if d.Engine == "postgres" {
		// Positional dbname avoids libpq interpreting '=' as connection options (rejected in Validate).
		args := append(options, "--no-psqlrc", "--tuples-only", "--no-align", "--dbname="+d.DBName, "--command=SHOW server_version")
		err = command(ctx, d, "psql", args, env, &out)
	} else {
		client = "mysqldump"
		query := "SELECT VERSION(); SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=CONVERT(0x" + hex.EncodeToString([]byte(d.DBName)) + " USING utf8mb4) AND TABLE_TYPE='BASE TABLE' AND ENGINE <> 'InnoDB';"
		err = command(ctx, d, "mysql", append(options, "--batch", "--skip-column-names", "--database="+d.DBName, "--execute="+query), env, &out)
	}
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	server := strings.TrimSpace(lines[0])
	if strings.Contains(strings.ToLower(server), "mariadb") {
		return "", fmt.Errorf("MariaDB não está homologado nesta versão; use um servidor MySQL")
	}
	if d.Engine == "mysql" && (len(lines) != 2 || strings.TrimSpace(lines[1]) != "0") {
		return "", fmt.Errorf("banco contém tabelas fora do InnoDB ou não foi possível verificar os mecanismos; backup consistente recusado")
	}
	var clientOut limitedBuffer
	if err = command(ctx, d, client, []string{"--version"}, env, &clientOut); err != nil {
		return "", err
	}
	sa, sb, err := version(server)
	if err != nil {
		return "", err
	}
	ca, cb, err := version(clientOut.String())
	if err != nil {
		return "", err
	}
	if d.Engine == "postgres" && (sa < 14 || sa > ca) {
		return "", fmt.Errorf("PostgreSQL %s incompatível com cliente %d; suportado de 14 a %d", server, ca, ca)
	}
	if d.Engine == "mysql" && (sa != ca || sb != cb || sa < 8) {
		return "", fmt.Errorf("MySQL %s exige cliente da mesma série; instalado %d.%d", server, ca, cb)
	}
	return fmt.Sprintf("Conexão OK · servidor %s · %s %d.%d", server, client, ca, cb), nil
}

func (NativeExecutor) Dump(ctx context.Context, d Database, w io.Writer) error {
	env, options, cleanup, err := credentials(d)
	if err != nil {
		return err
	}
	defer cleanup()
	if d.Engine == "postgres" {
		return command(ctx, d, "pg_dump", append(options, "--format=custom", "--compress=1", "--dbname="+d.DBName), env, w)
	}
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	err = command(ctx, d, "mysqldump", append(options, "--single-transaction", "--quick", "--routines", "--events", "--triggers", "--hex-blob", "--no-tablespaces", "--set-gtid-purged=OFF", "--column-statistics=0", d.DBName), env, gz)
	ce := gz.Close()
	if err != nil {
		return err
	}
	return ce
}

func InitDestination(path, name string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(path, ".baseguard-destination"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(name)
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}
func openDestination(destinations map[string]string, name string) (*os.Root, error) {
	p, ok := destinations[name]
	if !ok {
		return nil, fmt.Errorf("destino %q não configurado", name)
	}
	r, err := os.OpenRoot(p)
	if err != nil {
		return nil, fmt.Errorf("destino indisponível: %w", err)
	}
	b, err := r.ReadFile(".baseguard-destination")
	if err != nil || string(b) != name {
		r.Close()
		return nil, fmt.Errorf("destino indisponível ou sem marcador de montagem válido")
	}
	return r, nil
}

func writeBackup(ctx context.Context, r *os.Root, path string, d Database, ex Executor) (size int64, sum string, err error) {
	if _, e := r.Stat(path); !os.IsNotExist(e) {
		return 0, "", fmt.Errorf("arquivo de destino já existe ou não pode ser verificado")
	}
	f, err := r.OpenFile(path+".partial", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, "", err
	}
	defer func() {
		f.Close()
		if err != nil {
			_ = r.Remove(path + ".partial")
		}
	}()
	h := sha256.New()
	err = ex.Dump(ctx, d, io.MultiWriter(f, h))
	if err != nil {
		return
	}
	info, e := f.Stat()
	if e != nil {
		err = e
		return
	}
	size = info.Size()
	if size == 0 {
		err = fmt.Errorf("dump vazio")
		return
	}
	if err = f.Sync(); err != nil {
		return
	}
	if err = f.Close(); err != nil {
		return
	}
	if err = r.Rename(path+".partial", path); err != nil {
		return
	}
	// Persist the directory entry before acknowledging the backup in SQLite.
	if runtime.GOOS != "windows" {
		dir, e := r.Open(filepath.Dir(path))
		if e != nil {
			err = e
			return
		}
		err = dir.Sync()
		ce := dir.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			return
		}
	}
	sum = hex.EncodeToString(h.Sum(nil))
	return
}
