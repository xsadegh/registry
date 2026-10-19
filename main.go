package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

func main() {
	path := flag.String("config", os.Getenv("CONFIG_FILE"), "path to a YAML config file")
	flag.Parse()

	cfg, err := loadConfig(*path)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	fx.New(
		fx.Supply(cfg),
		fx.Provide(newLogger, openDriver, openStore, newRegistry, newServer, newHTTPServer),
		fx.WithLogger(newFxLogger),
		fx.Invoke(func(*http.Server) {}),
	).Run()
}

func newHTTPServer(lc fx.Lifecycle, sd fx.Shutdowner, cfg Config, s *server, log *zap.Logger) *http.Server {
	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          zap.NewStdLog(log.Named("http")),
	}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			if cfg.Server.TLSCert != "" {
				cert, err := tls.LoadX509KeyPair(cfg.Server.TLSCert, cfg.Server.TLSKey)
				if err != nil {
					return err
				}
				srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
			}
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return err
			}
			log.Info("listening", zap.String("addr", srv.Addr), zap.String("storage", cfg.location()), zap.Bool("tls", srv.TLSConfig != nil))
			go func() {
				var err error
				if srv.TLSConfig != nil {
					err = srv.ServeTLS(ln, "", "")
				} else {
					err = srv.Serve(ln)
				}
				if !errors.Is(err, http.ErrServerClosed) {
					log.Error("serve", zap.Error(err))
					_ = sd.Shutdown(fx.ExitCode(1))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("stopping")
			return srv.Shutdown(ctx)
		},
	})
	return srv
}
