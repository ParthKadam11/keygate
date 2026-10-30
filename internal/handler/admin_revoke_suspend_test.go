package handler

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/tabloy/keygate/internal/model"
)

// A revoke or suspend the database fails is a 500, not a 404 that tells
// the admin the licence is gone, and the database's message stays in
// the server log rather than the response.
func TestRevokeSuspendDatabaseError(t *testing.T) {
	f := newAPIFixture(t)
	h := &AdminHandler{Store: f.s}
	call := func(fn gin.HandlerFunc, id string) (int, map[string]any) {
		return f.call(fn, http.MethodPost, "/admin/licenses/"+id, "", nil, gin.Param{Key: "id", Value: id})
	}

	fn := "fail_admin_" + strings.ReplaceAll(f.licA.ID, "-", "")
	if _, err := f.s.DB.ExecContext(f.ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'database unavailable'; END $$`, fn)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.s.DB.ExecContext(f.ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON licenses", fn))
		_, _ = f.s.DB.ExecContext(f.ctx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s()", fn))
	})
	if _, err := f.s.DB.ExecContext(f.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON licenses FOR EACH ROW
		WHEN (OLD.id = '%s') EXECUTE FUNCTION %s()`, fn, f.licA.ID, fn)); err != nil {
		t.Fatal(err)
	}

	for name, fn := range map[string]gin.HandlerFunc{"revoke": h.RevokeLicense, "suspend": h.SuspendLicense} {
		code, out := call(fn, f.licA.ID)
		e, _ := out["error"].(map[string]any)
		if code != http.StatusInternalServerError || errCode(out) != "INTERNAL_ERROR" || strings.Contains(fmt.Sprint(e["message"]), "database unavailable") {
			t.Errorf("%s with the database failing = %d %v, want a 500 without the database's message", name, code, out)
		}
	}
	var st string
	if err := f.s.DB.NewRaw("SELECT status FROM licenses WHERE id = ?", f.licA.ID).Scan(f.ctx, &st); err != nil || st != model.StatusActive {
		t.Errorf("license status = %s (err %v), want it still active", st, err)
	}

	// Still a 404 for a licence that does not exist.
	for name, fn := range map[string]gin.HandlerFunc{"revoke": h.RevokeLicense, "suspend": h.SuspendLicense} {
		if code, out := call(fn, "no-such-license"); code != http.StatusNotFound {
			t.Errorf("%s of a missing license = %d %v, want 404", name, code, out)
		}
	}
}
