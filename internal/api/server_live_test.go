package api_test

// SCHED-GAP-204-A — the DB-free liveness route.
//
// TestLiveRoute_NoDBInteraction is the load-bearing proof for the route's
// contract: the Server is constructed with a *sql.DB whose driver PANICS on
// any use (Prepare/Ping/Query/Exec — anything database/sql can reach), so a
// single DB round-trip anywhere in the handler fails the test loudly. The
// route answers 200 anyway, proving it reads process memory only and is safe
// to call while the single SQLite connection is wedged or closed.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/api"
)

// panicConnector is a driver.Connector whose connections panic on ANY use.
// database/sql's optional interfaces (Pinger, Queryer, Execer) are all
// implemented on the connection too, so even the optimistic fast paths hit
// the panic instead of silently succeeding.
type panicConnector struct{}

type panicConn struct{}

var errDBMustNotBeUsed = errors.New("panicDB: /api/v1/live must not touch the database (SCHED-GAP-204-A)")

func boom() { panic(errDBMustNotBeUsed) }

func (panicConnector) Connect(context.Context) (driver.Conn, error) { return panicConn{}, nil }
func (panicConnector) Driver() driver.Driver                        { return panicDriver{} }

type panicDriver struct{}

func (panicDriver) Open(string) (driver.Conn, error) { return panicConn{}, nil }

func (panicConn) Prepare(string) (driver.Stmt, error) { boom(); return nil, nil }
func (panicConn) Close() error                        { boom(); return nil }
func (panicConn) Begin() (driver.Tx, error)           { boom(); return nil, nil }

func (panicConn) Ping(context.Context) error { boom(); return nil }
func (panicConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	boom()
	return nil, nil
}
func (panicConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	boom()
	return nil, nil
}

func (panicConn) Commit() error   { boom(); return nil }
func (panicConn) Rollback() error { boom(); return nil }

func TestLiveRoute_NoDBInteraction(t *testing.T) {
	db := sql.OpenDB(panicConnector{})
	t.Cleanup(func() { _ = db.Close() })

	// nil loop (unit-test shape): the handler must not need the loop either —
	// LastEvalTime takes a lock and is exactly what this route must avoid.
	srv := api.NewServer(db, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/v1/live")
	if err != nil {
		t.Fatalf("GET /api/v1/live: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the DB-free contract is broken", resp.StatusCode)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if body["status"] != "ok" {
		t.Errorf("status = %v, want \"ok\"", body["status"])
	}
	for _, field := range []string{"version", "build_sha", "uptime", "started"} {
		v, ok := body[field].(string)
		if !ok || v == "" {
			t.Errorf("%s = %v, want a non-empty string", field, body[field])
		}
	}
	if _, err := time.Parse(time.RFC3339, body["started"].(string)); err != nil {
		t.Errorf("started = %q is not RFC3339: %v", body["started"], err)
	}
}

// TestLiveRoute_MethodNotAllowed pins the 405 path: the probe is GET-only and
// must never panic on other methods.
func TestLiveRoute_MethodNotAllowed(t *testing.T) {
	db := sql.OpenDB(panicConnector{})
	t.Cleanup(func() { _ = db.Close() })

	srv := api.NewServer(db, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/v1/live", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/v1/live: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/v1/live status = %d, want 405", resp.StatusCode)
	}
}
