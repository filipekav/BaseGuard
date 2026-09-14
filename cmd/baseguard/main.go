package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"baseguard/internal/app"
	"github.com/gofrs/flock"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		slog.Error("BaseGuard encerrado", "error", err)
		os.Exit(1)
	}
}
func run() error {
	listen := env("BASEGUARD_LISTEN", ":8080")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		_, port, err := net.SplitHostPort(listen)
		if err != nil {
			return err
		}
		client := http.Client{Timeout: 3 * time.Second}
		r, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return fmt.Errorf("healthcheck: %d", r.StatusCode)
		}
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "init-destination" {
		if len(os.Args) != 4 {
			return fmt.Errorf("uso: baseguard init-destination /caminho Nome")
		}
		return app.InitDestination(os.Args[2], os.Args[3])
	}
	if len(os.Args) > 1 {
		return fmt.Errorf("comando desconhecido: %s", os.Args[1])
	}
	data := env("BASEGUARD_DATA_DIR", "/data")
	if err := os.MkdirAll(data, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(data, "baseguard.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("já existe uma instância utilizando este diretório de dados")
	}
	defer lock.Close()
	var destinations map[string]string
	if err = json.Unmarshal([]byte(env("BASEGUARD_DESTINATIONS", `{"Principal":"/backups"}`)), &destinations); err != nil {
		return fmt.Errorf("BASEGUARD_DESTINATIONS: JSON inválido (%w). Para o destino padrão Principal em /backups, remova essa variável das configurações do container e aplique a alteração", err)
	}
	if len(destinations) == 0 {
		return fmt.Errorf("configure pelo menos um destino")
	}
	for name, path := range destinations {
		if strings.TrimSpace(name) == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("destino deve ter nome e caminho absoluto")
		}
	}
	store, err := app.OpenStore(filepath.Join(data, "baseguard.db"), env("BASEGUARD_KEY_FILE", "/secrets/master.key"))
	if err != nil {
		return err
	}
	defer store.DB.Close()
	worker := &app.Worker{Store: store, Destinations: destinations, Executor: app.NativeExecutor{}}
	server, err := app.NewServer(store, worker, env("BASEGUARD_SECURE_COOKIES", "false") == "true")
	if err != nil {
		return err
	}
	password := os.Getenv("BASEGUARD_ADMIN_PASSWORD")
	if path := os.Getenv("BASEGUARD_ADMIN_PASSWORD_FILE"); path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		password = strings.TrimRight(string(b), "\r\n")
	}
	generated, err := server.Bootstrap(password)
	if err != nil {
		return err
	}
	if generated != "" {
		fmt.Fprintf(os.Stderr, "Primeiro acesso — usuário: admin | senha inicial: %s\nAltere a senha em Configurações.\n", generated)
	}
	if err = worker.Recover(); err != nil {
		return err
	}
	if err = worker.Schedule(time.Now(), "recovery"); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := worker.Start(ctx)
	httpServer := &http.Server{Addr: listen, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	errch := make(chan error, 1)
	go func() { slog.Info("BaseGuard disponível", "listen", listen); errch <- httpServer.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-errch:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	<-done
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
