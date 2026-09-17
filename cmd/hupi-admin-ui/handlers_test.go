package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/auth"
	"hupi/internal/crypto"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// these against a real Postgres instance; same HUPI_TEST_DATABASE_URL.
// This package's handlers have no logic of their own beyond routing and
// JSON marshaling (docs/ADMIN_UI.md § Verification) — every real
// assertion here is really exercising internal/auth.Store and
// internal/audit through the HTTP surface, the same way an API client
// would.

func testServer(t *testing.T) (*server, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	keys := crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
	var teamStore auth.TeamAuthenticator
	if auth.NewTeamAuthenticator != nil {
		teamStore = auth.NewTeamAuthenticator(db)
	}
	return &server{store: auth.New(db, keys), teamStore: teamStore, db: db}, db
}

func cleanupIDs(t *testing.T, db *sql.DB, userID, teamID string) {
	t.Helper()
	if userID != "" {
		db.Exec(`delete from api_keys where user_id = $1`, userID)
		db.Exec(`delete from team_members where user_id = $1`, userID)
		db.Exec(`delete from users where id = $1`, userID)
		db.Exec(`delete from scope_keys where scope_kind = 'private' and scope_owner = $1`, userID)
	}
	if teamID != "" {
		db.Exec(`delete from team_members where team_id = $1`, teamID)
		db.Exec(`delete from teams where id = $1`, teamID)
		db.Exec(`delete from scope_keys where scope_kind = 'shared' and scope_owner = $1`, teamID)
	}
}

// doJSON exercises the same http.Handler main.go wires up (srv.routes())
// so these tests fail if routing breaks, not just handler bodies in
// isolation. body, when non-nil, is marshaled to JSON as the request body.
func doJSON(t *testing.T, srv *server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	return rec
}

// decodeBody unmarshals rec.Body into v, failing the test with the raw
// body on any error — every JSON assertion below goes through this so a
// malformed response shows its own content in the failure message.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode JSON body: %v\nbody:\n%s", err, rec.Body.String())
	}
}

func TestListUsers(t *testing.T) {
	srv, db := testServer(t)
	ctx := context.Background()
	userID := "user:test-adminui-dash"
	t.Cleanup(func() { cleanupIDs(t, db, userID, "") })

	if err := srv.store.CreateUser(ctx, userID, "dash@example.com"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := doJSON(t, srv, http.MethodGet, "/api/users", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/users status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	var users []auth.User
	decodeBody(t, rec, &users)
	found := false
	for _, u := range users {
		if u.ID == userID {
			found = true
		}
	}
	if !found {
		t.Errorf("GET /api/users missing user %q, got %+v", userID, users)
	}
}

func TestUserDetail_UnknownUserReturns404(t *testing.T) {
	srv, _ := testServer(t)
	rec := doJSON(t, srv, http.MethodGet, "/api/users/user:does-not-exist-adminui", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["error"] == "" {
		t.Errorf("expected a JSON error body, got %+v", body)
	}
}

func TestCreateUser_MissingID(t *testing.T) {
	srv, _ := testServer(t)
	rec := doJSON(t, srv, http.MethodPost, "/api/users", map[string]string{"email": "x@example.com"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["error"] == "" {
		t.Errorf("expected a JSON error body, got %+v", body)
	}
}

// TestUserDetail_EmailRoundTrips is what TestUserDetail_EscapesEmail used
// to check before this package spoke JSON instead of HTML: arbitrary
// operator-supplied text (an email containing HTML-special characters)
// must come back byte-for-byte, since a JSON API has no HTML-escaping
// step to regress in the first place — the point that mattered about the
// old test.
func TestUserDetail_EmailRoundTrips(t *testing.T) {
	srv, db := testServer(t)
	userID := "user:test-adminui-roundtrip"
	t.Cleanup(func() { cleanupIDs(t, db, userID, "") })

	const email = `<script>alert(1)</script>`
	if err := srv.store.CreateUser(context.Background(), userID, email); err != nil {
		t.Fatalf("create user: %v", err)
	}

	rec := doJSON(t, srv, http.MethodGet, "/api/users/"+userID, nil)
	var detail struct {
		User auth.User `json:"user"`
	}
	decodeBody(t, rec, &detail)
	if detail.User.Email != email {
		t.Errorf("email round-tripped as %q, want %q", detail.User.Email, email)
	}
}

func TestUnauthenticatedRequest_Rejected(t *testing.T) {
	srv, _ := testServer(t)
	handler := requireOperatorAuth(srv.store, srv.routes())

	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestRequireOperatorAuth exercises the named-operator auth model
// (docs/GAP_CLOSURE_PLAN.md §4.3) end to end against real Postgres:
// creating an operator, resolving their token, and the username-mismatch
// safety check.
func TestRequireOperatorAuth(t *testing.T) {
	srv, db := testServer(t)
	ctx := context.Background()
	name := "test-adminui-op-alice"
	// Pre-cleanup, not just t.Cleanup: an interrupted prior run (killed
	// process, panic before Cleanup fires) leaves this row behind, and
	// CreateOperator's primary key would then collide on the next run —
	// operators.name has no equivalent of the scope-keyed tables' natural
	// per-test uniqueness, so this test has to make its own.
	db.Exec(`delete from operators where name = $1`, name)
	t.Cleanup(func() { db.Exec(`delete from operators where name = $1`, name) })

	token, err := srv.store.CreateOperator(ctx, name)
	if err != nil {
		t.Fatalf("create operator: %v", err)
	}

	revokedName := "test-adminui-op-carol"
	db.Exec(`delete from operators where name = $1`, revokedName)
	t.Cleanup(func() { db.Exec(`delete from operators where name = $1`, revokedName) })
	revokedToken, err := srv.store.CreateOperator(ctx, revokedName)
	if err != nil {
		t.Fatalf("create operator to revoke: %v", err)
	}
	if err := srv.store.RevokeOperator(ctx, revokedName); err != nil {
		t.Fatalf("revoke operator: %v", err)
	}

	handler := requireOperatorAuth(srv.store, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resolved-Operator", operatorFromContext(r.Context()))
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name       string
		user, pass string
		setAuth    bool
		wantStatus int
	}{
		{"correct token, no username", "", token, true, http.StatusOK},
		{"correct token, matching username", name, token, true, http.StatusOK},
		{"correct token, mismatched username", "someone-else", token, true, http.StatusUnauthorized},
		{"wrong token", "", "hupi_op_wrong", true, http.StatusUnauthorized},
		{"revoked token", "", revokedToken, true, http.StatusUnauthorized},
		{"no Authorization header", "", "", false, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
			if tc.setAuth {
				req.SetBasicAuth(tc.user, tc.pass)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				if got := rec.Header().Get("X-Resolved-Operator"); got != name {
					t.Errorf("resolved operator = %q, want %q", got, name)
				}
			}
		})
	}
}

// TestWhoami confirms GET /api/whoami reports the resolved operator name
// off the request context requireOperatorAuth sets — the frontend's way
// of confirming stored credentials still work.
func TestWhoami(t *testing.T) {
	srv, _ := testServer(t)
	opName := "test-adminui-whoami"
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil).WithContext(
		context.WithValue(context.Background(), operatorContextKey{}, opName))
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeBody(t, rec, &body)
	if body["name"] != opName {
		t.Errorf("whoami name = %q, want %q", body["name"], opName)
	}
}

// TestOperatorLifecycle_CreateListRevoke covers the new operator
// management endpoints, which no HTML UI ever exposed (only
// hupi-admin's create-operator/revoke-operator CLI subcommands).
func TestOperatorLifecycle_CreateListRevoke(t *testing.T) {
	srv, db := testServer(t)
	name := fmt.Sprintf("test-adminui-op-lifecycle-%d", time.Now().UnixNano())
	t.Cleanup(func() { db.Exec(`delete from operators where name = $1`, name) })

	rec := doJSON(t, srv, http.MethodPost, "/api/operators", map[string]string{"name": name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create operator status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	var createResp struct {
		RawToken string `json:"raw_token"`
	}
	decodeBody(t, rec, &createResp)
	if !strings.HasPrefix(createResp.RawToken, "hupi_op_") {
		t.Fatalf("raw_token %q doesn't look like an operator token", createResp.RawToken)
	}

	if _, err := srv.store.ResolveOperator(context.Background(), createResp.RawToken); err != nil {
		t.Errorf("issued operator token does not resolve: %v", err)
	}

	rec = doJSON(t, srv, http.MethodGet, "/api/operators", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list operators status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	var operators []auth.Operator
	decodeBody(t, rec, &operators)
	found := false
	for _, op := range operators {
		if op.Name == name {
			found = true
			if op.RevokedAt != nil {
				t.Errorf("newly created operator shows as revoked: %+v", op)
			}
		}
	}
	if !found {
		t.Fatalf("GET /api/operators missing %q, got %+v", name, operators)
	}

	rec = doJSON(t, srv, http.MethodPost, "/api/operators/revoke", map[string]string{"name": name})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke operator status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	if _, err := srv.store.ResolveOperator(context.Background(), createResp.RawToken); err == nil {
		t.Error("revoked operator token still resolves successfully")
	}
}

func TestCreateOperator_MissingName(t *testing.T) {
	srv, _ := testServer(t)
	rec := doJSON(t, srv, http.MethodPost, "/api/operators", map[string]string{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestAuditQuery seeds a handful of known audit-generating actions
// (attributed to a per-run-unique operator, since audit_log rows are
// undeletable by the app role — schema/0007's comment — so a fixed name
// would accumulate rows across runs) and confirms GET /api/audit both
// returns them unfiltered and that an actor filter actually filters,
// rather than just happening to return everything.
func TestAuditQuery(t *testing.T) {
	srv, db := testServer(t)
	ctx := context.Background()
	opName := fmt.Sprintf("test-adminui-op-audit-%d", time.Now().UnixNano())
	otherOpName := fmt.Sprintf("test-adminui-op-audit-other-%d", time.Now().UnixNano())
	userID := "user:test-adminui-audit-query"
	t.Cleanup(func() {
		db.Exec(`delete from operators where name = any($1)`, []string{opName, otherOpName})
		cleanupIDs(t, db, userID, "")
	})

	if _, err := srv.store.CreateOperator(ctx, opName); err != nil {
		t.Fatalf("create operator: %v", err)
	}
	if _, err := srv.store.CreateOperator(ctx, otherOpName); err != nil {
		t.Fatalf("create operator: %v", err)
	}

	withOperator := func(name string, req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), operatorContextKey{}, name))
	}

	// One admin_provision event (create-user) attributed to opName, and one
	// unrelated admin_provision event (a create-operator call, via the
	// store directly to attribute it to otherOpName) that a filter on
	// opName must exclude.
	req := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewReader(mustJSON(t, map[string]string{
		"id": userID, "email": "audit-query@example.com",
	})))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, withOperator(opName, req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user status = %d, body:\n%s", rec.Code, rec.Body.String())
	}

	unrelatedName := fmt.Sprintf("test-adminui-op-audit-created-%d", time.Now().UnixNano())
	req = httptest.NewRequest(http.MethodPost, "/api/operators", bytes.NewReader(mustJSON(t, map[string]string{
		"name": unrelatedName,
	})))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, withOperator(otherOpName, req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create operator (unrelated) status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { db.Exec(`delete from operators where name = $1`, unrelatedName) })

	// Unfiltered-by-actor query with a generous limit should see both
	// actors' rows.
	rec = doJSON(t, srv, http.MethodGet, "/api/audit?event_type=admin_provision&limit=500", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/audit status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	var all []auditEntryForTest
	decodeBody(t, rec, &all)
	seenOp, seenOther := false, false
	for _, e := range all {
		if e.Actor == opName {
			seenOp = true
		}
		if e.Actor == otherOpName {
			seenOther = true
		}
	}
	if !seenOp || !seenOther {
		t.Fatalf("expected rows from both actors in unfiltered query; seenOp=%v seenOther=%v", seenOp, seenOther)
	}

	// Filtered by actor=opName must return only that actor's rows — the
	// part that actually proves filtering works, not just that an
	// unfiltered call returns everything.
	rec = doJSON(t, srv, http.MethodGet, "/api/audit?actor="+opName, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/audit?actor=... status = %d, body:\n%s", rec.Code, rec.Body.String())
	}
	var filtered []auditEntryForTest
	decodeBody(t, rec, &filtered)
	if len(filtered) == 0 {
		t.Fatal("actor-filtered query returned no rows")
	}
	for _, e := range filtered {
		if e.Actor != opName {
			t.Errorf("actor-filtered query returned a row for %q, want only %q", e.Actor, opName)
		}
	}
}

// auditEntryForTest mirrors internal/audit.LogEntry's JSON shape without
// importing the package just for a field-name match in tests — keeps this
// file's dependency list to what it already has.
type auditEntryForTest struct {
	Actor     string `json:"Actor"`
	EventType string `json:"EventType"`
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestAdminUIActions_AreAudited confirms every hook added in
// docs/GAP_CLOSURE_PLAN.md §4.3 actually fires — a view (admin_ui_view)
// and a provisioning action (admin_provision), each attributed to the
// authenticated operator, not a shared token.
func TestAdminUIActions_AreAudited(t *testing.T) {
	srv, db := testServer(t)
	ctx := context.Background()
	// audit_log rows are intentionally undeletable by the app role — see
	// schema/0007_audit_log.sql's comment (an audit trail the same role
	// generating events can also erase isn't one). That means a fixed
	// actor name here would accumulate a growing count across every past
	// test run instead of the exactly-one this test checks for; a
	// per-run-unique name sidesteps that without fighting the schema.
	opName := fmt.Sprintf("test-adminui-op-bob-%d", time.Now().UnixNano())
	userID := "user:test-adminui-audit"
	t.Cleanup(func() {
		db.Exec(`delete from operators where name = $1`, opName)
		cleanupIDs(t, db, userID, "")
	})

	if _, err := srv.store.CreateOperator(ctx, opName); err != nil {
		t.Fatalf("create operator: %v", err)
	}

	// createUser/userDetail call operatorFromContext directly, so drive
	// them through a request carrying that value the same way
	// requireOperatorAuth would set it — no need to also re-prove Basic
	// Auth parsing here, TestRequireOperatorAuth already does that.
	withOperator := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), operatorContextKey{}, opName))
	}

	body := mustJSON(t, map[string]string{"id": userID, "email": "audit@example.com"})
	req := httptest.NewRequest(http.MethodPost, "/api/users", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, withOperator(req))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create user status = %d, body:\n%s", rec.Code, rec.Body.String())
	}

	var provisionCount int
	if err := db.QueryRowContext(ctx, `
		select count(*) from audit_log where actor = $1 and event_type = 'admin_provision'
	`, opName).Scan(&provisionCount); err != nil {
		t.Fatalf("count admin_provision events: %v", err)
	}
	if provisionCount != 1 {
		t.Errorf("got %d admin_provision audit rows, want 1", provisionCount)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/users/"+userID, nil)
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, withOperator(req))
	if rec.Code != http.StatusOK {
		t.Fatalf("user detail status = %d", rec.Code)
	}

	var viewCount int
	if err := db.QueryRowContext(ctx, `
		select count(*) from audit_log where actor = $1 and event_type = 'admin_ui_view'
	`, opName).Scan(&viewCount); err != nil {
		t.Fatalf("count admin_ui_view events: %v", err)
	}
	if viewCount != 1 {
		t.Errorf("got %d admin_ui_view audit rows, want 1", viewCount)
	}
}
