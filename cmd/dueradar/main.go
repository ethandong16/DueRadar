package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ethandong16/due-radar/internal/core"
	"github.com/ethandong16/due-radar/internal/instance"
	"github.com/ethandong16/due-radar/internal/server"
	"github.com/ethandong16/due-radar/internal/vault"
	"github.com/ethandong16/due-radar/web"
)

func main() {
	listen := flag.String("listen", env("DUERADAR_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	dir := flag.String("data", env("DUERADAR_DATA_DIR", "data"), "private persistent data directory")
	demo := flag.Bool("demo", false, "use labeled sample tasks; do not send notifications")
	secure := flag.Bool("secure-cookies", os.Getenv("DUERADAR_SECURE_COOKIES") == "true", "require HTTPS for admin cookies")
	version := flag.Bool("version", false, "print version")
	health := flag.String("healthcheck", "", "check a running health URL and exit")
	flag.Parse()
	if *version {
		fmt.Println(server.Version)
		return
	}
	if *health != "" {
		client := http.Client{Timeout: 5 * time.Second}
		r, err := client.Get(*health)
		if err != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			os.Exit(1)
		}
		fmt.Println("ok")
		return
	}
	if *demo {
		*dir = filepath.Join(*dir, "demo")
	}
	if len(os.Getenv("DUERADAR_ADMIN_PASSWORD")) < 12 {
		log.Fatal("set DUERADAR_ADMIN_PASSWORD to a password of at least 12 characters")
	}
	owner, err := instance.Acquire(*dir)
	if err != nil {
		log.Fatal(err)
	}
	defer owner.Close()
	engine, err := core.Open(filepath.Join(*dir, "state.json"))
	if err != nil {
		log.Fatal(err)
	}
	credentials, err := vault.Open(*dir)
	if err != nil {
		log.Fatal(err)
	}
	app, err := server.New(engine, credentials, os.Getenv("DUERADAR_ADMIN_PASSWORD"), *demo, *secure)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *demo {
		if err := app.Sync(ctx); err != nil {
			log.Fatal(err)
		}
	}
	httpServer := &http.Server{Addr: *listen, Handler: app.Handler(web.Files), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	go app.Run(ctx)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		backgroundDone := make(chan error, 1)
		go func() { backgroundDone <- app.Shutdown(shutdown) }()
		if err := httpServer.Shutdown(shutdown); err != nil {
			log.Print("HTTP shutdown deadline reached")
		}
		if err := <-backgroundDone; err != nil {
			log.Print("Background shutdown deadline reached")
		}
	}()
	log.Printf("DueRadar %s listening on %s (demo=%t)", server.Version, *listen, *demo)
	serveErr := httpServer.ListenAndServe()
	if err := serveErr; err != nil && err != http.ErrServerClosed {
		log.Print(err)
		stop()
	}
	<-shutdownDone
	if serveErr != nil && serveErr != http.ErrServerClosed {
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}
