package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"dbcat/internal/db"
)

// Categories of the object list besides tables/views.
const (
	catIndexes  = "indexes"
	catTriggers = "triggers"
	catRoutines = "routines"
)

// treeObj is one leaf of the lazily loaded tree groups.
type treeObj struct {
	Ref
	Kind  string // index | trigger | function | procedure
	ID    string
	Name  string
	Tbl   string
	Label string
	Sub   string
	Icon  string
}

func routineLabel(x db.RoutineInfo) string { return x.Name + "(" + x.Args + ")" }

func (s *Server) objectData(ctx context.Context, w http.ResponseWriter, r *http.Request) (*db.Conn, Ref, string, bool) {
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return nil, ref, "", false
	}
	ref.Schema = effectiveSchema(c, ref)
	return c, ref, r.FormValue("cat"), true
}

func (s *Server) treeCategory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	c, err := s.conn(ctx, ref.Conn)
	if err != nil {
		s.fail(w, "tree-error", err)
		return
	}
	ref.Schema = effectiveSchema(c, ref)
	var items []treeObj
	switch r.FormValue("cat") {
	case catIndexes:
		list, err := c.IndexList(ctx, ref.dbRef())
		if err != nil {
			s.fail(w, "tree-error", err)
			return
		}
		for _, x := range list {
			items = append(items, treeObj{Ref: ref, Kind: db.KindIndex, ID: x.ID, Name: x.Name, Tbl: x.Table, Label: x.Name, Sub: x.Table, Icon: "index"})
		}
	case catTriggers:
		list, err := c.TriggerList(ctx, ref.dbRef())
		if err != nil {
			s.fail(w, "tree-error", err)
			return
		}
		for _, x := range list {
			items = append(items, treeObj{Ref: ref, Kind: db.KindTrigger, ID: x.ID, Name: x.Name, Tbl: x.Table, Label: x.Name, Sub: x.Table, Icon: "trigger"})
		}
	case catRoutines:
		list, err := c.RoutineList(ctx, ref.dbRef())
		if err != nil {
			s.fail(w, "tree-error", err)
			return
		}
		for _, x := range list {
			icon := "func"
			if x.Kind == db.KindProcedure {
				icon = "proc"
			}
			items = append(items, treeObj{Ref: ref, Kind: x.Kind, ID: x.ID, Name: x.Name, Label: routineLabel(x), Icon: icon})
		}
	default:
		s.fail(w, "tree-error", errors.New("알 수 없는 분류"))
		return
	}
	s.render(w, "tree-objs", items)
}

type catView struct {
	Ref
	Cat      string
	Indexes  []db.IndexInfo
	Triggers []db.TriggerInfo
	Routines []db.RoutineInfo
	Err      string
}

// objectCategory renders the object-list body of the indexes / triggers / routines tabs.
func (s *Server) objectCategory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	c, ref, cat, ok := s.objectData(ctx, w, r)
	if !ok {
		return
	}
	v := catView{Ref: ref, Cat: cat}
	var err error
	switch cat {
	case catIndexes:
		v.Indexes, err = c.IndexList(ctx, ref.dbRef())
	case catTriggers:
		v.Triggers, err = c.TriggerList(ctx, ref.dbRef())
	case catRoutines:
		v.Routines, err = c.RoutineList(ctx, ref.dbRef())
	default:
		err = errors.New("알 수 없는 분류")
	}
	if err != nil {
		v.Err = err.Error()
	}
	s.render(w, "objcat", v)
}

type defView struct {
	Ref
	Kind string
	Name string
	DDL  string
	Err  string
}

// objectDef renders a definition tab (read-only SQL with copy / open-in-editor).
func (s *Server) objectDef(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metaTimeout)
	defer cancel()
	ref := refFrom(r)
	kind, id := r.FormValue("kind"), r.FormValue("id")
	v := defView{Ref: ref, Kind: kind, Name: r.FormValue("name")}
	c, err := s.conn(ctx, ref.Conn)
	if err == nil {
		ref.Schema = effectiveSchema(c, ref)
		v.Ref = ref
		if kind == "table" || kind == "view" {
			ref.Table = id
			v.DDL, err = c.DDL(ctx, ref.dbRef())
		} else {
			v.DDL, err = c.ObjectDDL(ctx, ref.dbRef(), kind, id)
		}
	}
	if err != nil {
		v.Err = err.Error()
	}
	s.render(w, "objdef", v)
}

type objInfoView struct {
	Ref
	Err   string
	Props [][2]string
}

// objectKindInfo fills the information pane for an index, trigger or routine.
func (s *Server) objectKindInfo(ctx context.Context, c *db.Conn, ref Ref, kind, id, tab string) (string, any) {
	if tab == "ddl" {
		ddl, err := c.ObjectDDL(ctx, ref.dbRef(), kind, id)
		if err != nil {
			return "objinfo-ddl", map[string]any{"Err": err.Error()}
		}
		return "objinfo-ddl", infoView{Ref: ref, DDL: ddl}
	}
	v := objInfoView{Ref: ref}
	found := false
	switch kind {
	case db.KindIndex:
		list, err := c.IndexList(ctx, ref.dbRef())
		if err != nil {
			return "objinfo-props", map[string]any{"Err": err.Error()}
		}
		for _, x := range list {
			if x.ID == id {
				found = true
				typ := "일반"
				if x.Primary {
					typ = "기본키"
				} else if x.Unique {
					typ = "고유"
				}
				v.Props = [][2]string{{"이름", x.Name}, {"유형", "인덱스 (" + typ + ")"}, {"테이블", x.Table}, {"필드", strings.ReplaceAll(x.Columns, "`", "")}}
			}
		}
	case db.KindTrigger:
		list, err := c.TriggerList(ctx, ref.dbRef())
		if err != nil {
			return "objinfo-props", map[string]any{"Err": err.Error()}
		}
		for _, x := range list {
			if x.ID == id {
				found = true
				v.Props = [][2]string{{"이름", x.Name}, {"유형", "트리거"}, {"테이블", x.Table}, {"이벤트", x.Event}}
			}
		}
	default:
		list, err := c.RoutineList(ctx, ref.dbRef())
		if err != nil {
			return "objinfo-props", map[string]any{"Err": err.Error()}
		}
		for _, x := range list {
			if x.ID == id {
				found = true
				label := "함수 (FUNCTION)"
				if x.Kind == db.KindProcedure {
					label = "프로시저 (PROCEDURE)"
				}
				v.Props = [][2]string{{"이름", x.Name}, {"유형", label}, {"인수", orDash(x.Args)}, {"반환", orDash(x.Returns)}, {"언어", orDash(x.Language)}}
			}
		}
	}
	if !found {
		v.Err = "오브젝트를 찾을 수 없습니다 (삭제되었거나 이름이 바뀌었습니다)"
	}
	return "objinfo-props", v
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
