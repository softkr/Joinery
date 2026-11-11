package db

import (
	"context"
	"strings"
	"testing"
)

func TestTriggerEvent(t *testing.T) {
	for in, want := range map[string]string{
		"CREATE TRIGGER trg AFTER UPDATE OF status ON orders BEGIN SELECT 1; END":                         "AFTER UPDATE",
		"CREATE TRIGGER t BEFORE INSERT ON x FOR EACH ROW BEGIN END":                                      "BEFORE INSERT",
		`CREATE TRIGGER "t" INSTEAD OF DELETE ON v BEGIN SELECT 1; END`:                                   "INSTEAD OF DELETE",
		"CREATE TRIGGER t AFTER INSERT OR UPDATE OR DELETE ON public.x FOR EACH ROW EXECUTE FUNCTION f()": "AFTER INSERT OR UPDATE OR DELETE",
		"CREATE TRIGGER t\n  BEFORE   INSERT\n ON x BEGIN END":                                            "BEFORE INSERT",
		"garbage": "",
	} {
		if got := triggerEvent(in); got != want {
			t.Errorf("triggerEvent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestObjectListsSQLite(t *testing.T) {
	ctx := context.Background()
	c := openTestDB(t)
	for _, q := range []string{
		"CREATE INDEX idx_users_name ON users(name)",
		"CREATE UNIQUE INDEX idx_users_name_age ON users(name, age)",
		"CREATE TABLE log(msg TEXT)",
		"CREATE TRIGGER trg_users AFTER INSERT ON users BEGIN INSERT INTO log VALUES (NEW.name); END",
	} {
		if _, _, err := c.RunScript(ctx, Ref{}, q); err != nil {
			t.Fatal(err)
		}
	}

	idx, err := c.IndexList(ctx, Ref{})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]IndexInfo{}
	for _, x := range idx {
		by[x.Name] = x
	}
	if x := by["idx_users_name"]; x.Table != "users" || x.Columns != "name" || x.Unique {
		t.Errorf("idx_users_name = %+v", x)
	}
	if x := by["idx_users_name_age"]; x.Columns != "name, age" || !x.Unique {
		t.Errorf("idx_users_name_age = %+v", x)
	}
	if _, ok := by["sqlite_autoindex_nopk_1"]; ok {
		t.Error("unexpected index")
	}

	trg, err := c.TriggerList(ctx, Ref{})
	if err != nil || len(trg) != 1 || trg[0].Name != "trg_users" || trg[0].Table != "users" || trg[0].Event != "AFTER INSERT" {
		t.Fatalf("triggers = %+v, %v", trg, err)
	}
	if r, err := c.RoutineList(ctx, Ref{}); err != nil || len(r) != 0 || c.HasRoutines() {
		t.Errorf("SQLite has no routines: %+v %v", r, err)
	}

	ddl, err := c.ObjectDDL(ctx, Ref{}, KindIndex, "idx_users_name_age")
	if err != nil || !strings.Contains(ddl, "CREATE UNIQUE INDEX idx_users_name_age") {
		t.Errorf("index ddl = %q, %v", ddl, err)
	}
	ddl, err = c.ObjectDDL(ctx, Ref{}, KindTrigger, "trg_users")
	if err != nil || !strings.Contains(ddl, "CREATE TRIGGER trg_users") || !strings.HasSuffix(ddl, ";") {
		t.Errorf("trigger ddl = %q, %v", ddl, err)
	}
	// the index created by UNIQUE on a column has no stored definition
	if _, _, err := c.RunScript(ctx, Ref{}, "CREATE TABLE u(a TEXT UNIQUE)"); err != nil {
		t.Fatal(err)
	}
	if ddl, err = c.ObjectDDL(ctx, Ref{}, KindIndex, "sqlite_autoindex_u_1"); err != nil || !strings.Contains(ddl, "자동") {
		t.Errorf("auto index ddl = %q, %v", ddl, err)
	}
	for name, args := range map[string][2]string{
		"unknown kind": {"table", "x"}, "missing": {KindIndex, "nope"}, "routine on sqlite": {KindFunction, "f"},
	} {
		if _, err := c.ObjectDDL(ctx, Ref{}, args[0], args[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
