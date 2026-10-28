// Command muse2api serves the OpenAI-compatible API, runs the admin console and
// provides the local cookie-helper / mail.tm utilities.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/muse2api/muse2api/internal/api"
	"github.com/muse2api/muse2api/internal/config"
	"github.com/muse2api/muse2api/internal/engine"
	"github.com/muse2api/muse2api/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			serve()
			return
		case "cookie-helper":
			os.Exit(runCookieHelper(os.Args[2:]))
		case "mailtm-otp":
			os.Exit(runMailTMOTP(os.Args[2:]))
		case "verify-selectors":
			os.Exit(runVerifySelectors(os.Args[2:]))
		case "version", "-v", "--version":
			fmt.Println(config.DefaultVersion)
			return
		case "help", "-h", "--help":
			usage()
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
			usage()
			os.Exit(2)
		}
	}
	serve()
}

func usage() {
	fmt.Print(`muse2api - OpenAI-compatible gateway for muse.ai

Usage:
  muse2api [serve]                 Start the API server and admin console
  muse2api cookie-helper [flags]   Import muse.ai cookies with a local browser
  muse2api mailtm-otp [flags]      Create a mail.tm inbox and read a login code
  muse2api verify-selectors [flags] Inspect a running muse.ai debug page
  muse2api version                 Print the version

Environment:
  MUSE2API_KEY, MUSE2API_HOST, MUSE2API_PORT, MUSE2API_PUBLIC_BASE,
  MUSE2API_CHROMIUM, MUSE2API_CDP_PORT, MUSE2API_CONCURRENCY,
  MUSE2API_IMAGE_TIMEOUT, MUSE2API_VIDEO_TIMEOUT, MUSE2API_CHAT_TIMEOUT,
  MUSE2API_REF_IMAGE_LOCAL_FILES, MUSE2API_REF_IMAGE_PRIVATE_FETCH,
  MUSE2API_REPO, MUSE2API_REPO_BRANCH, MUSE2API_REBUILD_CMD, GITHUB_TOKEN

The service reads .env from MUSE2API_HOME (default: the executable directory).
`)
}

func serve() {
	cfg := config.Load()
	if err := cfg.EnsureDirs(); err != nil {
		log.Fatalf("failed to create data directories: %v", err)
	}
	if cfg.APIKey == "" {
		cfg.APIKey = "m2a_" + randomToken(24)
		if err := cfg.PersistEnv("MUSE2API_KEY", cfg.APIKey); err != nil {
			log.Fatalf("could not persist the generated API key: %v", err)
		}
		log.Printf("No MUSE2API_KEY was configured; generated an initial key: %s", cfg.APIKey)
	}

	st, err := store.New(cfg.AccountsFile(), cfg.TasksFile())
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if n, err := st.MarkInterruptedTasks(); err != nil {
		log.Fatalf("recover tasks: %v", err)
	} else if n > 0 {
		log.Printf("marked %d interrupted generation task(s) as failed", n)
	}
	eng := engine.New(cfg)
	srv := api.NewServer(cfg, st, eng)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.StartBackground(ctx)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       180 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("muse2api %s listening on http://%s:%d (admin: /admin)", cfg.Version, cfg.Host, cfg.Port)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Printf("shutdown requested")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server stopped: %v", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	eng.Stop()
	log.Printf("bye")
}

func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
