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
			env = append(env, "PGSSLROOTCERT=/etc/ssl/certs/ca-certificates.crt")
		}
		options = []string{"--host=" + d.Host, "--port=" + strconv.Itoa(d.Port), "--username=" + d.Username, "--no-password"}
	} else {
		esc := func(s string) string { return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(s) }
		content = "[client]\npassword=\"" + esc(d.Password) + "\"\n"
		tls := map[string]string{"disable": "DISABLED", "require": "REQUIRED", "verify-full": "VERIFY_IDENTITY"}[d.TLSMode]
		options = []string{"--defaults-file=" + f.Name(), "--protocol=TCP", "--host=" + d.Host, "--port=" + strconv.Itoa(d.Port), "--user=" + d.Username}
		if d.Engine == "mariadb" {
			switch d.TLSMode {
			case "disable":
				options = append(options, "--skip-ssl")
			case "require":
				options = append(options, "--ssl", "--disable-ssl-verify-server-cert")
			case "verify-full":
				options = append(options, "--ssl", "--ssl-verify-server-cert")
			}
		} else {
			options = append(options, "--ssl-mode="+tls)
			if d.TLSMode == "disable" {
				// caching_sha2_password needs RSA exchange when TLS is explicitly disabled.
				options = append(options, "--get-server-public-key")
			}
		}
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
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
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

func dumpWithProfile(ctx context.Context, d Database, p clientProfile, w io.Writer) error {
	env, options, cleanup, err := credentials(d)
	if err != nil {
		return err
	}
	defer cleanup()
	if d.Engine == "postgres" {
		return command(ctx, d, p.binary(p.Dump), append(options, "--format=custom", "--compress=1", "--dbname="+d.DBName), env, w)
	}
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	dumpArgs := []string{"--single-transaction", "--quick", "--routines", "--events", "--triggers", "--hex-blob", "--no-tablespaces"}
	if d.Engine == "mysql" {
		dumpArgs = append(dumpArgs, "--set-gtid-purged=OFF", "--column-statistics=0")
	}
	err = command(ctx, d, p.binary(p.Dump), append(append(options, dumpArgs...), d.DBName), env, gz)
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
	f, err := os.OpenFile(filepath.Join(path, ".baseguard-destination"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
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
