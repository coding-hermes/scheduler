package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

func TestUsagePoolDeferralSurfaces(t *testing.T) {
	a := newAPITestServer(t)
	if err := database.ConfigureUsagePools(context.Background(), a.db, []database.UsagePool{{
		ID: "host:build-01", Kind: "host", ActiveLimit: 8, Enabled: true,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(a.ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/status = %d", resp.StatusCode)
	}
	var body struct {
		UsagePools []database.UsagePoolLease `json:"usage_pools"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.UsagePools) != 1 {
		t.Fatalf("usage_pools = %+v, want one configured pool", body.UsagePools)
	}
	pool := body.UsagePools[0]
	if pool.PoolID != "host:build-01" || pool.Kind != "host" || pool.Active != 0 || pool.Limit != 8 || pool.Available != 8 {
		t.Fatalf("usage pool status = %+v", pool)
	}
}
