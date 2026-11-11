package web

import (
	"net/http"
	"strings"
	"time"

	"dbcat/internal/store"
)

const (
	historyListLimit = 200
	previewRunes     = 400
)

// SetHistory enables query history (nil disables the feature).
func (s *Server) SetHistory(h *store.History) { s.history = h }

// recordHistory stores a run. Failures to write the history never affect the
// query result the user is waiting for.
func (s *Server) recordHistory(ref Ref, sql string, ok bool, took time.Duration) {
	if s.history == nil {
		return
	}
	_ = s.history.Add(store.HistoryEntry{
		Conn: ref.Conn, DB: ref.DB, Schema: ref.Schema, SQL: sql,
		At: time.Now(), OK: ok, Ms: took.Milliseconds(),
	})
}

type historyItem struct {
	ID       string
	ConnName string
	Where    string
	Preview  string
	At       time.Time
	OK       bool
	Ms       int64
}

type historyView struct {
	Conn    string
	Scope   string
	Enabled bool
	Items   []historyItem
}

func preview(sql string) string {
	flat := strings.Join(strings.Fields(sql), " ")
	r := []rune(flat)
	if len(r) > previewRunes {
		return string(r[:previewRunes]) + " …"
	}
	return flat
}

func (s *Server) historyItems(scope, q string) []historyItem {
	names := map[string]string{}
	for _, c := range s.store.List() {
		names[c.ID] = c.Name
	}
	var items []historyItem
	for _, e := range s.history.List(scope, q, historyListLimit) {
		where := e.DB
		if e.Schema != "" {
			where += "." + e.Schema
		}
		name := names[e.Conn]
		if name == "" {
			name = "(삭제된 연결)"
		}
		items = append(items, historyItem{
			ID: e.ID, ConnName: name, Where: where, Preview: preview(e.SQL), At: e.At, OK: e.OK, Ms: e.Ms,
		})
	}
	return items
}

func (s *Server) historyDialog(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.NotFound(w, r)
		return
	}
	conn := r.FormValue("c")
	s.render(w, "history", historyView{Conn: conn, Scope: conn, Enabled: s.history.Enabled(), Items: s.historyItems(conn, "")})
}

func (s *Server) historyList(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.NotFound(w, r)
		return
	}
	scope := r.FormValue("scope")
	s.render(w, "history-list", historyView{Scope: scope, Items: s.historyItems(scope, r.FormValue("q"))})
}

// historySQL returns the full text of one entry (the list only shows a preview).
func (s *Server) historySQL(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.NotFound(w, r)
		return
	}
	e, ok := s.history.Get(r.FormValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(e.SQL))
}

func (s *Server) historyDelete(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.history.Delete(r.FormValue("id")); err != nil {
		toast(w, "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
	}
	// empty body: htmx swaps the row (outerHTML) away
}

func (s *Server) historyClear(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.NotFound(w, r)
		return
	}
	scope := r.FormValue("scope")
	if err := s.history.Clear(scope); err != nil {
		toast(w, "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	toast(w, "ok", "기록을 삭제했습니다")
	s.render(w, "history-list", historyView{Scope: scope})
}

func (s *Server) historyEnable(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		http.NotFound(w, r)
		return
	}
	on := r.FormValue("on") == "1"
	if err := s.history.SetEnabled(on); err != nil {
		toast(w, "error", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	msg := "이제 실행한 쿼리를 기록합니다"
	if !on {
		msg = "쿼리 기록을 중단했습니다 (기존 기록은 유지됩니다)"
	}
	toast(w, "ok", msg)
}
