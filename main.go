package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"dbcat/internal/store"
	"dbcat/internal/web"
)

// App is bound into the webview; it exposes native dialogs the page cannot open itself.
type App struct {
	ctx context.Context
	srv *web.Server
}

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

// PickFile opens the native file chooser (SQLite database or SSH key).
func (a *App) PickFile(title string) (string, error) {
	return wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title:           title,
		ShowHiddenFiles: true, // SSH keys usually live in ~/.ssh
	})
}

// saveDialog asks where to save; an empty path means the user cancelled.
func (a *App) saveDialog(name string) (string, error) {
	return wruntime.SaveFileDialog(a.ctx, wruntime.SaveDialogOptions{Title: "내보내기", DefaultFilename: name})
}

// SaveText writes client-generated text (query result export) to a file the
// user picks. It returns the chosen path, or "" when cancelled.
func (a *App) SaveText(name, content string) (string, error) {
	path, err := a.saveDialog(filepath.Base(name))
	if err != nil || path == "" {
		return "", err
	}
	return path, os.WriteFile(path, []byte(content), 0o600)
}

// SaveBase64 writes binary content (the ER diagram PNG) given as base64 to a
// file the user picks. It returns the chosen path, or "" when cancelled.
func (a *App) SaveBase64(name, b64 string) (string, error) {
	const maxBytes = 64 << 20
	if base64.StdEncoding.DecodedLen(len(b64)) > maxBytes {
		return "", errors.New("file is too large")
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", err
	}
	path, err := a.saveDialog(filepath.Base(name))
	if err != nil || path == "" {
		return "", err
	}
	return path, os.WriteFile(path, data, 0o600)
}

const (
	exportPrefix = "/table/export?"
	backupPrefix = "/backup/dump?"
)

// ExportURL streams a table export or a database backup (the same request the
// browser would download) into a file the user picks. It returns the chosen
// path, or "" when cancelled.
func (a *App) ExportURL(rel string) (string, error) {
	var (
		name  string
		write func(context.Context, string, io.Writer) error
	)
	switch {
	case strings.HasPrefix(rel, exportPrefix):
		rel = strings.TrimPrefix(rel, exportPrefix)
		name, write = a.srv.ExportName(rel), a.srv.ExportTo
	case strings.HasPrefix(rel, backupPrefix):
		rel = strings.TrimPrefix(rel, backupPrefix)
		name, write = a.srv.DumpName(rel), a.srv.DumpTo
	default:
		return "", errors.New("unsupported export URL")
	}
	path, err := a.saveDialog(name)
	if err != nil || path == "" {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Minute)
	defer cancel()
	if err := write(ctx, rel, f); err != nil {
		f.Close()
		os.Remove(path) // never leave a truncated export behind
		return "", err
	}
	return path, f.Close()
}

func main() {
	webAddr := flag.String("web", "", "serve the UI over loopback HTTP (e.g. 127.0.0.1:8080) instead of opening the window")
	cfgPath := flag.String("config", "", "connections file (default: <user config dir>/dbcat/connections.json)")
	histPath := flag.String("history", "", "query history file (default: <user config dir>/dbcat/history.json)")
	flag.Parse()

	var (
		st  *store.Store
		err error
	)
	if *cfgPath != "" {
		st, err = store.OpenAt(*cfgPath)
	} else {
		st, err = store.Open()
	}
	if err != nil {
		log.Fatalf("connection store: %v", err)
	}
	srv, err := web.New(st)
	if err != nil {
		log.Fatalf("ui: %v", err)
	}
	defer srv.Close()

	// History is optional: a broken history file must not stop the app.
	var hist *store.History
	if *histPath != "" {
		hist, err = store.OpenHistoryAt(*histPath)
	} else {
		hist, err = store.OpenHistory()
	}
	if err != nil {
		log.Printf("query history disabled: %v", err)
	} else {
		srv.SetHistory(hist)
	}

	if *webAddr != "" {
		if err := serveWeb(*webAddr, srv); err != nil {
			log.Fatal(err)
		}
		return
	}

	app := &App{srv: srv}
	err = wails.Run(&options.App{
		Title:     "Joinery",
		Width:     1360,
		Height:    860,
		MinWidth:  960,
		MinHeight: 560,
		AssetServer: &assetserver.Options{
			Handler: srv.Handler(),
		},
		BackgroundColour: &options.RGBA{R: 243, G: 245, B: 248, A: 255},
		OnStartup:        app.startup,
		OnShutdown:       func(context.Context) { srv.Close() },
		Bind:             []any{app},
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
		},
	})
	if err != nil {
		log.Fatalf("wails: %v", err)
	}
}

// serveWeb runs the UI on a loopback address only: the app has no
// authentication, so it must never be reachable from the network.
func serveWeb(addr string, srv *web.Server) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-web: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("-web: only loopback addresses are allowed (127.0.0.1, ::1, localhost)")
	}
	hs := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}()
	log.Printf("Joinery UI on http://%s", addr)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
