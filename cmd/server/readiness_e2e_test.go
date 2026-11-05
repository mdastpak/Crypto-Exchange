package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"encrypted-db/internal/db"
	"encrypted-db/internal/testutil/tcpproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadiness_FollowsPostgreSQL serves the readiness probe with the real
// services, but reaches PostgreSQL through a TCP proxy, which stops and
// starts it as far as the server can tell.
func TestReadiness_FollowsPostgreSQL(t *testing.T) {
	env := newE2EEnv(t)
	proxy, err := tcpproxy.New("localhost:5432")
	require.NoError(t, err)
	t.Cleanup(proxy.Close)
	conn, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%s user=postgres password=test dbname=%s sslmode=disable",
		proxy.Host(), proxy.Port(), e2eDBName))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	system := env.handlers.System
	saved := system.Postgres
	t.Cleanup(func() { system.Postgres = saved })
	system.Postgres = &db.PostgresService{DB: conn}
	router := newTestRouter(t, env)

	probe := func(path string) int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w.Code
	}

	require.Equal(t, http.StatusOK, probe("/system/ready"), "ready while every dependency is up")

	proxy.SetDown(true)
	proxy.Cut()
	assert.Equal(t, http.StatusServiceUnavailable, probe("/system/ready"), "PostgreSQL is down")
	assert.Equal(t, http.StatusServiceUnavailable, probe("/system/healthcheck"))
	assert.Equal(t, http.StatusOK, probe("/system/live"), "the process itself is still alive")

	proxy.SetDown(false)
	assert.Equal(t, http.StatusOK, probe("/system/ready"), "ready again once PostgreSQL is back")
}
