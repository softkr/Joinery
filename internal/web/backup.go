package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dbcat/internal/db"
)

const (
	maxRestoreBytes = 256 << 20
	restoreTimeout  = 60 * time.Minute
)

type backupView struct {
	Ref
	Tables  []string
	Views   int
	HasObjs bool
	Err     string
}

func (s *Server) backupDialog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "backup-dialog", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	tabs, err := c.Tables(ctx, ref.dbRef())
	if err != nil {
		s.fail(w, "backup-dialog", err)
		return
	}
	v := backupView{Ref: ref, HasObjs: true}
	for _, t := range tabs {
		if t.IsView {
			v.Views++
		} else {
			v.Tables = append(v.Tables, t.Name)
		}
	}
	s.render(w, "backup-dialog", v)
}

func (s *Server) restoreDialog(w http.ResponseWriter, r *http.Request) {
	ref := refFrom(r)
	name := ref.DB
	if ref.Schema != "" {
		name += "." + ref.Schema
	}
	cfg, _ := s.store.Get(ref.Conn)
	s.render(w, "restore-dialog", map[string]any{"Ref": ref, "Target": cfg.Name + " › " + name})
}

func parseDump(q url.Values) (Ref, db.DumpOptions) {
	ref := Ref{Conn: q.Get("c"), DB: q.Get("d"), Schema: q.Get("s")}
	return ref, db.DumpOptions{
		Tables: q["t"], NoTables: q.Get("nt") == "1", Views: q.Get("views") == "1", Structure: q.Get("struct") == "1",
		Data: q.Get("data") == "1", Drop: q.Get("drop") == "1", Objects: q.Get("objs") == "1",
	}
}

// DumpName is the suggested file name of a backup request.
func (s *Server) DumpName(rawQuery string) string {
	q, _ := url.ParseQuery(rawQuery)
	ref, _ := parseDump(q)
	base := ref.Schema
	if base == "" {
		base = ref.DB
	}
	if base == "" || base == "main" {
		if cfg, ok := s.store.Get(ref.Conn); ok && cfg.Name != "" {
			base = cfg.Name
		}
	}
	return fmt.Sprintf("%s-%s.sql", sanitizeName(base), time.Now().Format("20060102-150405"))
}

func sanitizeName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127 {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "backup"
	}
	return sb.String()
}

// DumpTo writes the backup described by the query string to w.
func (s *Server) DumpTo(ctx context.Context, rawQuery string, w io.Writer) error {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return err
	}
	ref, opts := parseDump(q)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		return err
	}
	ref.Schema = effectiveSchema(c, ref)
	return c.Dump(ctx, ref.dbRef(), opts, w)
}

func (s *Server) backupDump(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), exportTimeout)
	defer cancel()
	hw := &headerWriter{w: w, ctype: "application/sql; charset=utf-8", name: s.DumpName(r.URL.RawQuery)}
	if err := s.DumpTo(ctx, r.URL.RawQuery, hw); err != nil {
		if !hw.written {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		log.Printf("backup %q aborted: %v", r.URL.RawQuery, err)
	}
}

type restoreView struct {
	Err     string
	Summary *db.ExecSummary
	File    string
	Elapsed string
	OK      bool
}

func (s *Server) restoreRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), restoreTimeout)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, maxRestoreBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.render(w, "restore-result", restoreView{Err: "파일을 받지 못했습니다 (최대 256MB): " + err.Error()})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		s.render(w, "restore-result", restoreView{Err: "SQL 파일을 선택하세요"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxRestoreBytes+1))
	if err != nil || len(data) > maxRestoreBytes {
		s.render(w, "restore-result", restoreView{Err: "파일이 너무 큽니다 (최대 256MB)"})
		return
	}
	script := string(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")))

	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.render(w, "restore-result", restoreView{Err: err.Error()})
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	sum, err := c.ExecScript(ctx, ref.dbRef(), script, r.FormValue("cont") == "1")
	if err != nil {
		s.render(w, "restore-result", restoreView{Err: err.Error()})
		return
	}
	events(w, map[string]any{"schema-changed": refEvent(ref), "rows-changed": refEvent(ref)})
	s.render(w, "restore-result", restoreView{
		Summary: sum, File: hdr.Filename, Elapsed: fmt.Sprintf("%.2f", sum.Elapsed.Seconds()), OK: sum.Failed == 0,
	})
}
