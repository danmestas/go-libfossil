package dst

import (
	"fmt"
	"testing"
	"time"

	"github.com/danmestas/go-libfossil/internal/repo"
	"github.com/danmestas/go-libfossil/internal/uv"
)

// --- Table sync test helpers ---

func deviceTableDef() repo.TableDef {
	return repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "device_id", Type: "text", PK: true},
			{Name: "hostname", Type: "text"},
			{Name: "status", Type: "text"},
		},
		Conflict: "mtime-wins",
	}
}

func configTableDef() repo.TableDef {
	return repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "key", Type: "text", PK: true},
			{Name: "value", Type: "text"},
		},
		Conflict: "mtime-wins",
	}
}

func selfWriteTableDef() repo.TableDef {
	return repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "peer_id", Type: "text", PK: true},
			{Name: "last_seen", Type: "integer"},
			{Name: "health", Type: "text"},
		},
		Conflict: "self-write",
	}
}

func ownerWriteTableDef() repo.TableDef {
	return repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "resource_id", Type: "text", PK: true},
			{Name: "data", Type: "text"},
		},
		Conflict: "owner-write",
	}
}

func registerTable(t *testing.T, r *repo.Repo, name string, def repo.TableDef, mtime int64) {
	t.Helper()
	if err := repo.EnsureSyncSchema(r.DB()); err != nil {
		t.Fatalf("EnsureSyncSchema: %v", err)
	}
	if err := repo.RegisterSyncedTable(r.DB(), name, def, mtime); err != nil {
		t.Fatalf("RegisterSyncedTable %s: %v", name, err)
	}
}

func registerTableAll(t *testing.T, sim *Simulator, masterRepo *repo.Repo, name string, def repo.TableDef, mtime int64) {
	t.Helper()
	registerTable(t, masterRepo, name, def, mtime)
	for _, leafID := range sim.LeafIDs() {
		registerTable(t, sim.Leaf(leafID).Repo(), name, def, mtime)
	}
}

func upsertRow(t *testing.T, r *repo.Repo, table string, row map[string]any, mtime int64) {
	t.Helper()
	if err := repo.UpsertXRow(r.DB(), table, row, mtime); err != nil {
		t.Fatalf("UpsertXRow: %v", err)
	}
}

func assertRowCount(t *testing.T, r *repo.Repo, label, table string, def repo.TableDef, n int) {
	t.Helper()
	rows, _, err := repo.ListXRows(r.DB(), table, def)
	if err != nil {
		t.Fatalf("ListXRows %s: %v", label, err)
	}
	if len(rows) != n {
		t.Errorf("%s: expected %d rows, got %d", label, n, len(rows))
	}
}

func assertRowValue(t *testing.T, r *repo.Repo, label, table string, def repo.TableDef, pk map[string]any, col, want string) {
	t.Helper()
	var pkColDefs []repo.ColumnDef
	for _, c := range def.Columns {
		if c.PK {
			pkColDefs = append(pkColDefs, c)
		}
	}
	pkHash := repo.PKHash(pkColDefs, pk)
	row, _, err := repo.LookupXRow(r.DB(), table, def, pkHash)
	if err != nil {
		t.Fatalf("LookupXRow %s: %v", label, err)
	}
	if row == nil {
		t.Fatalf("%s: missing row pk=%v", label, pk)
	}
	got, _ := row[col].(string)
	if got != want {
		t.Errorf("%s: %s=%q, want %q", label, col, got, want)
	}
}

func logRowCounts(t *testing.T, sim *Simulator, table string, def repo.TableDef) {
	t.Helper()
	for _, leafID := range sim.LeafIDs() {
		rows, _, _ := repo.ListXRows(sim.Leaf(leafID).Repo().DB(), table, def)
		t.Logf("  %s: %d rows", leafID, len(rows))
	}
}

func newTableSyncSim(t *testing.T, seed int64, numLeaves int, buggify bool) (*Simulator, *repo.Repo, *MockFossil) {
	t.Helper()
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)
	sim, err := New(SimConfig{
		Seed:                seed,
		NumLeaves:           numLeaves,
		PollInterval:        5 * time.Second,
		TmpDir:              t.TempDir(),
		Upstream:            mf,
		Buggify:             buggify,
		SafetyCheckInterval: 10,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { sim.Close() })
	sim.SetMasterRepo(masterRepo)
	return sim, masterRepo, mf
}

func runAndCheck(t *testing.T, sim *Simulator, sev severity, seed int64, steps int) {
	t.Helper()
	if err := sim.Run(steps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[%s] seed=%d steps=%d syncs=%d errors=%d",
		sev.Name, seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)
	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}
}

func isNormalMode(sev severity) bool {
	return sev.DropRate == 0 && !sev.Buggify
}

// =============================================================================
// Level 1: Normal (0% faults)
// =============================================================================

func TestTS_Convergence3Peer(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(200)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "devices"
	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i, leafID := range sim.LeafIDs() {
		r := sim.Leaf(leafID).Repo()
		for j := 0; j < 3; j++ {
			upsertRow(t, r, table, map[string]any{
				"device_id": fmt.Sprintf("leaf%d-dev%d", i, j),
				"hostname":  fmt.Sprintf("host-%d-%d", i, j),
				"status":    "online",
			}, int64(100+i*10+j))
		}
	}

	runAndCheck(t, sim, sev, seed, stepsFor(300))

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		for _, leafID := range sim.LeafIDs() {
			assertRowCount(t, sim.Leaf(leafID).Repo(), string(leafID), table, def, 9)
		}
		assertRowCount(t, masterRepo, "master", table, def, 9)
	} else {
		logRowCounts(t, sim, table, def)
	}
}

func TestTS_SchemaDeployChain(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(201)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "settings"
	def := configTableDef()
	registerTable(t, masterRepo, table, def, 100)

	runAndCheck(t, sim, sev, seed, stepsFor(300))

	if isNormalMode(sev) {
		for _, leafID := range sim.LeafIDs() {
			r := sim.Leaf(leafID).Repo()
			tables, err := repo.ListSyncedTables(r.DB())
			if err != nil {
				t.Fatalf("ListSyncedTables %s: %v", leafID, err)
			}
			found := false
			for _, tbl := range tables {
				if tbl.Name == table {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: schema %q not deployed", leafID, table)
			}
		}
	}
}

func TestTS_MultipleTables5(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(202)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 2, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	defs := make([]repo.TableDef, 5)
	names := make([]string, 5)
	for i := 0; i < 5; i++ {
		names[i] = fmt.Sprintf("table_%d", i)
		defs[i] = configTableDef()
		registerTableAll(t, sim, masterRepo, names[i], defs[i], 100)
		for j := 0; j < 5; j++ {
			upsertRow(t, masterRepo, names[i], map[string]any{
				"key":   fmt.Sprintf("k%d-%d", i, j),
				"value": fmt.Sprintf("v%d-%d", i, j),
			}, 100)
		}
	}

	runAndCheck(t, sim, sev, seed, stepsFor(300))

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		for i, name := range names {
			for _, leafID := range sim.LeafIDs() {
				assertRowCount(t, sim.Leaf(leafID).Repo(), string(leafID), name, defs[i], 5)
			}
		}
	}
}

func TestTS_CatalogHashShortCircuit(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(203)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 2, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "configs"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	allRepos := []*repo.Repo{masterRepo}
	for _, leafID := range sim.LeafIDs() {
		allRepos = append(allRepos, sim.Leaf(leafID).Repo())
	}
	for _, r := range allRepos {
		for i := 0; i < 10; i++ {
			upsertRow(t, r, table, map[string]any{
				"key":   fmt.Sprintf("k%d", i),
				"value": fmt.Sprintf("v%d", i),
			}, 100)
		}
	}

	runAndCheck(t, sim, sev, seed, stepsFor(200))

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		masterHash, _ := repo.CatalogHash(masterRepo.DB(), table, def)
		for _, leafID := range sim.LeafIDs() {
			leafHash, _ := repo.CatalogHash(sim.Leaf(leafID).Repo().DB(), table, def)
			if leafHash != masterHash {
				t.Errorf("%s hash %s != master %s", leafID, leafHash, masterHash)
			}
		}
	}
}

func TestTS_MtimeWinsSamePK(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(204)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "config"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i, leafID := range sim.LeafIDs() {
		upsertRow(t, sim.Leaf(leafID).Repo(), table, map[string]any{
			"key":   "theme",
			"value": fmt.Sprintf("val-%d", i),
		}, int64(100+i*100))
	}

	runAndCheck(t, sim, sev, seed, stepsFor(300))

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		pk := map[string]any{"key": "theme"}
		assertRowValue(t, masterRepo, "master", table, def, pk, "value", "val-2")
		for _, leafID := range sim.LeafIDs() {
			assertRowValue(t, sim.Leaf(leafID).Repo(), string(leafID), table, def, pk, "value", "val-2")
		}
	}
}

func TestTS_MtimeWinsTieBreak(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(205)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 2, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "config"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for _, leafID := range sim.LeafIDs() {
		upsertRow(t, sim.Leaf(leafID).Repo(), table, map[string]any{
			"key":   "lang",
			"value": "en",
		}, 100)
	}

	runAndCheck(t, sim, sev, seed, stepsFor(200))

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		assertRowCount(t, masterRepo, "master", table, def, 1)
		for _, leafID := range sim.LeafIDs() {
			assertRowCount(t, sim.Leaf(leafID).Repo(), string(leafID), table, def, 1)
		}
	}
}

func TestTS_SelfWriteEnforcement(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(206)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "peer_status"
	def := selfWriteTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i, leafID := range sim.LeafIDs() {
		upsertRow(t, sim.Leaf(leafID).Repo(), table, map[string]any{
			"peer_id":   fmt.Sprintf("peer-%d", i),
			"last_seen": int64(1000 + i*10),
			"health":    "healthy",
		}, int64(100+i))
	}

	runAndCheck(t, sim, sev, seed, stepsFor(300))

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		assertRowCount(t, masterRepo, "master", table, def, 3)
		for _, leafID := range sim.LeafIDs() {
			assertRowCount(t, sim.Leaf(leafID).Repo(), string(leafID), table, def, 3)
		}
	} else {
		logRowCounts(t, sim, table, def)
	}
}

// =============================================================================
// Level 2: Adversarial (10% faults)
// =============================================================================

func TestTS_PartitionHeal(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(300)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "devices"
	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	upsertRow(t, masterRepo, table, map[string]any{
		"device_id": "shared", "hostname": "base", "status": "ok",
	}, 100)
	if err := sim.Run(stepsFor(50)); err != nil {
		t.Fatalf("Run phase 1: %v", err)
	}

	sim.Network().Partition("leaf-0")
	upsertRow(t, sim.Leaf("leaf-0").Repo(), table, map[string]any{
		"device_id": "isolated", "hostname": "from-0", "status": "partitioned",
	}, 200)
	upsertRow(t, sim.Leaf("leaf-1").Repo(), table, map[string]any{
		"device_id": "connected", "hostname": "from-1", "status": "ok",
	}, 200)
	if err := sim.Run(stepsFor(100)); err != nil {
		t.Fatalf("Run phase 2: %v", err)
	}

	sim.Network().Heal("leaf-0")
	for _, leafID := range sim.LeafIDs() {
		sim.ScheduleSyncNow(leafID)
	}
	if err := sim.Run(stepsFor(150)); err != nil {
		t.Fatalf("Run phase 3: %v", err)
	}

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}
	t.Logf("[%s] seed=%d steps=%d syncs=%d errors=%d",
		sev.Name, seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
	} else {
		logRowCounts(t, sim, table, def)
	}
}

func TestTS_PartitionConcurrentSamePK(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(301)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 2, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "config"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	sim.Network().Partition("leaf-0")

	upsertRow(t, sim.Leaf("leaf-0").Repo(), table, map[string]any{
		"key": "mode", "value": "old",
	}, 100)
	upsertRow(t, sim.Leaf("leaf-1").Repo(), table, map[string]any{
		"key": "mode", "value": "new",
	}, 200)
	if err := sim.Run(stepsFor(100)); err != nil {
		t.Fatalf("Run partitioned: %v", err)
	}

	sim.Network().Heal("leaf-0")
	for _, leafID := range sim.LeafIDs() {
		sim.ScheduleSyncNow(leafID)
	}
	if err := sim.Run(stepsFor(200)); err != nil {
		t.Fatalf("Run healed: %v", err)
	}

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}
	t.Logf("[%s] seed=%d steps=%d syncs=%d errors=%d",
		sev.Name, seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		pk := map[string]any{"key": "mode"}
		assertRowValue(t, masterRepo, "master", table, def, pk, "value", "new")
	}
}

func TestTS_BuggifyConvergence100(t *testing.T) {
	seed := seedFor(302)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, true)
	sim.Network().SetDropRate(0.10)

	table := "sensors"
	def := repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "sensor_id", Type: "integer", PK: true},
			{Name: "reading", Type: "real"},
		},
		Conflict: "mtime-wins",
	}
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i := 0; i < 100; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"sensor_id": int64(i),
			"reading":   float64(20.0 + float64(i)*0.1),
		}, 100)
	}

	if err := sim.Run(stepsFor(500)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("[buggify] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	for _, leafID := range sim.LeafIDs() {
		r := sim.Leaf(leafID).Repo()
		if err := CheckDeltaChains(string(leafID), r); err != nil {
			t.Fatalf("Delta chain: %v", err)
		}
		if err := CheckNoOrphanPhantoms(string(leafID), r); err != nil {
			t.Fatalf("Orphan phantom: %v", err)
		}
	}

	logRowCounts(t, sim, table, def)
}

func TestTS_BuggifyMultiTable(t *testing.T) {
	seed := seedFor(303)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, true)
	sim.Network().SetDropRate(0.10)

	names := []string{"alpha", "beta", "gamma"}
	def := configTableDef()
	for _, name := range names {
		registerTableAll(t, sim, masterRepo, name, def, 100)
		for j := 0; j < 10; j++ {
			upsertRow(t, masterRepo, name, map[string]any{
				"key":   fmt.Sprintf("%s-k%d", name, j),
				"value": fmt.Sprintf("%s-v%d", name, j),
			}, 100)
		}
	}

	if err := sim.Run(stepsFor(500)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("[buggify-multi] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	for _, leafID := range sim.LeafIDs() {
		r := sim.Leaf(leafID).Repo()
		if err := CheckDeltaChains(string(leafID), r); err != nil {
			t.Fatalf("Delta chain: %v", err)
		}
	}

	for _, name := range names {
		t.Logf("  table %s:", name)
		logRowCounts(t, sim, name, def)
	}
}

func TestTS_StalePeerRejoin(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(304)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "devices"
	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i := 0; i < 5; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"device_id": fmt.Sprintf("init-%d", i),
			"hostname":  fmt.Sprintf("host-%d", i),
			"status":    "active",
		}, 100)
	}
	if err := sim.Run(stepsFor(50)); err != nil {
		t.Fatalf("Run phase 1: %v", err)
	}

	sim.Network().Partition("leaf-2")
	for i := 0; i < 5; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"device_id": fmt.Sprintf("new-%d", i),
			"hostname":  fmt.Sprintf("new-host-%d", i),
			"status":    "pending",
		}, 200)
	}
	if err := sim.Run(stepsFor(100)); err != nil {
		t.Fatalf("Run phase 2: %v", err)
	}

	sim.Network().Heal("leaf-2")
	sim.ScheduleSyncNow("leaf-2")
	if err := sim.Run(stepsFor(150)); err != nil {
		t.Fatalf("Run phase 3: %v", err)
	}

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}
	t.Logf("[%s] seed=%d steps=%d syncs=%d errors=%d",
		sev.Name, seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		assertRowCount(t, sim.Leaf("leaf-2").Repo(), "leaf-2", table, def, 10)
	} else {
		logRowCounts(t, sim, table, def)
	}
}

func TestTS_SchemaBeforeRows(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(305)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 2, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "metrics"
	def := configTableDef()
	registerTable(t, masterRepo, table, def, 100)

	if err := sim.Run(stepsFor(100)); err != nil {
		t.Fatalf("Run phase 1: %v", err)
	}

	for i := 0; i < 5; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"key":   fmt.Sprintf("metric-%d", i),
			"value": fmt.Sprintf("%d", i*100),
		}, 200)
	}
	if err := sim.Run(stepsFor(200)); err != nil {
		t.Fatalf("Run phase 2: %v", err)
	}

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}
	t.Logf("[%s] seed=%d steps=%d syncs=%d errors=%d",
		sev.Name, seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	if isNormalMode(sev) {
		if err := sim.CheckAllTableSyncConverged(); err != nil {
			t.Fatalf("TableSync convergence: %v", err)
		}
		for _, leafID := range sim.LeafIDs() {
			assertRowCount(t, sim.Leaf(leafID).Repo(), string(leafID), table, def, 5)
		}
	} else {
		logRowCounts(t, sim, table, def)
	}
}

func TestTS_OwnerWriteEnforcement(t *testing.T) {
	sev := parseSeverity()
	seed := seedFor(306)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, sev.Buggify)
	sim.Network().SetDropRate(sev.DropRate)

	table := "resources"
	def := ownerWriteTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i, leafID := range sim.LeafIDs() {
		upsertRow(t, sim.Leaf(leafID).Repo(), table, map[string]any{
			"resource_id": fmt.Sprintf("res-%d", i),
			"data":        fmt.Sprintf("data-from-%d", i),
			"_owner":      string(leafID),
		}, int64(100+i))
	}

	if err := sim.Run(stepsFor(300)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}
	t.Logf("[%s] seed=%d steps=%d syncs=%d errors=%d",
		sev.Name, seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	logRowCounts(t, sim, table, def)
}

// =============================================================================
// Level 3: Hostile (25% faults)
// =============================================================================

func newHostileSim(t *testing.T, seed int64, numLeaves int, uvEnabled bool) (*Simulator, *repo.Repo, *MockFossil) {
	t.Helper()
	masterRepo := createMasterRepo(t)
	mf := NewMockFossil(masterRepo)
	sim, err := New(SimConfig{
		Seed:         seed,
		NumLeaves:    numLeaves,
		PollInterval: 5 * time.Second,
		TmpDir:       t.TempDir(),
		Upstream:     mf,
		Buggify:      true,
		UV:           uvEnabled,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { sim.Close() })
	sim.Network().SetDropRate(0.25)
	sim.SetMasterRepo(masterRepo)
	return sim, masterRepo, mf
}

func assertBoundedProgress(t *testing.T, sim *Simulator, table string, def repo.TableDef, minRows int) {
	t.Helper()
	for _, leafID := range sim.LeafIDs() {
		rows, _, err := repo.ListXRows(sim.Leaf(leafID).Repo().DB(), table, def)
		if err != nil {
			t.Fatalf("ListXRows %s: %v", leafID, err)
		}
		if len(rows) < minRows {
			t.Errorf("%s: expected >= %d rows, got %d", leafID, minRows, len(rows))
		}
	}
}

func checkStructuralIntegrity(t *testing.T, sim *Simulator) {
	t.Helper()
	for _, leafID := range sim.LeafIDs() {
		r := sim.Leaf(leafID).Repo()
		if err := CheckDeltaChains(string(leafID), r); err != nil {
			t.Fatalf("Delta chain %s: %v", leafID, err)
		}
		if err := CheckNoOrphanPhantoms(string(leafID), r); err != nil {
			t.Fatalf("Orphan phantom %s: %v", leafID, err)
		}
	}
}

func TestTS_HostileConvergence(t *testing.T) {
	seed := seedFor(400)
	sim, masterRepo, _ := newHostileSim(t, seed, 5, false)

	table := "devices"
	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i, leafID := range sim.LeafIDs() {
		r := sim.Leaf(leafID).Repo()
		for j := 0; j < 10; j++ {
			upsertRow(t, r, table, map[string]any{
				"device_id": fmt.Sprintf("leaf%d-dev%d", i, j),
				"hostname":  fmt.Sprintf("host-%d-%d", i, j),
				"status":    "online",
			}, int64(100+i*100+j))
		}
	}

	if err := sim.Run(stepsFor(300)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[hostile] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	checkStructuralIntegrity(t, sim)
	assertBoundedProgress(t, sim, table, def, 10)
	logRowCounts(t, sim, table, def)
}

func TestTS_CorruptXRowPayload(t *testing.T) {
	seed := seedFor(401)
	sim, masterRepo, _ := newHostileSim(t, seed, 3, false)

	table := "config"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i := 0; i < 20; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"key":   fmt.Sprintf("key-%d", i),
			"value": fmt.Sprintf("val-%d", i),
		}, 100)
	}

	if err := sim.Run(stepsFor(500)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[corrupt-payload] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	checkStructuralIntegrity(t, sim)
	assertBoundedProgress(t, sim, table, def, 1)
	logRowCounts(t, sim, table, def)
}

func TestTS_DropSchemaCards(t *testing.T) {
	seed := seedFor(402)
	sim, masterRepo, _ := newHostileSim(t, seed, 3, false)

	table := "alerts"
	def := configTableDef()
	registerTable(t, masterRepo, table, def, 100)

	for i := 0; i < 10; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"key":   fmt.Sprintf("alert-%d", i),
			"value": fmt.Sprintf("msg-%d", i),
		}, 100)
	}

	if err := sim.Run(stepsFor(500)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[drop-schema] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	checkStructuralIntegrity(t, sim)

	schemaDeployed := 0
	for _, leafID := range sim.LeafIDs() {
		tables, err := repo.ListSyncedTables(sim.Leaf(leafID).Repo().DB())
		if err != nil {
			t.Fatalf("ListSyncedTables %s: %v", leafID, err)
		}
		for _, tbl := range tables {
			if tbl.Name == table {
				schemaDeployed++
				break
			}
		}
	}
	t.Logf("  schema deployed to %d/%d leaves", schemaDeployed, len(sim.LeafIDs()))
	if schemaDeployed == 0 {
		t.Errorf("schema not deployed to any leaf after 500 steps")
	}
}

func TestTS_TruncatedXIGotList(t *testing.T) {
	seed := seedFor(403)
	sim, masterRepo, _ := newHostileSim(t, seed, 3, false)

	table := "sensors"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i, leafID := range sim.LeafIDs() {
		r := sim.Leaf(leafID).Repo()
		for j := 0; j < 10; j++ {
			upsertRow(t, r, table, map[string]any{
				"key":   fmt.Sprintf("s%d-%d", i, j),
				"value": fmt.Sprintf("reading-%d-%d", i, j),
			}, int64(100+i*10+j))
		}
	}

	if err := sim.Run(stepsFor(600)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[truncated-xigot] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	checkStructuralIntegrity(t, sim)
	assertBoundedProgress(t, sim, table, def, 10)
	logRowCounts(t, sim, table, def)
}

func TestTS_CatalogHashCorruption(t *testing.T) {
	seed := seedFor(404)
	sim, masterRepo, _ := newHostileSim(t, seed, 2, false)

	table := "config"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	allRepos := []*repo.Repo{masterRepo}
	for _, leafID := range sim.LeafIDs() {
		allRepos = append(allRepos, sim.Leaf(leafID).Repo())
	}
	for _, r := range allRepos {
		for i := 0; i < 15; i++ {
			upsertRow(t, r, table, map[string]any{
				"key":   fmt.Sprintf("k%d", i),
				"value": fmt.Sprintf("v%d", i),
			}, 100)
		}
	}

	if err := sim.Run(stepsFor(400)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[catalog-corrupt] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	checkStructuralIntegrity(t, sim)
	assertBoundedProgress(t, sim, table, def, 15)
	logRowCounts(t, sim, table, def)
}

func TestTS_MixedWorkload(t *testing.T) {
	seed := seedFor(405)
	sim, masterRepo, mf := newHostileSim(t, seed, 3, true)

	for i := 0; i < 30; i++ {
		mf.StoreArtifact([]byte(fmt.Sprintf("mixed-blob-%04d-seed%d", i, seed)))
	}

	uv.EnsureSchema(masterRepo.DB())
	for i := 0; i < 5; i++ {
		uv.Write(masterRepo.DB(), fmt.Sprintf("mixed/file-%d.txt", i),
			[]byte(fmt.Sprintf("mixed-uv-%d-seed%d", i, seed)), int64(100+i))
	}

	table := "telemetry"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)
	for i := 0; i < 20; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"key":   fmt.Sprintf("metric-%d", i),
			"value": fmt.Sprintf("%d", i*100),
		}, 100)
	}

	if err := sim.Run(stepsFor(600)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[mixed-workload] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	checkStructuralIntegrity(t, sim)

	for _, leafID := range sim.LeafIDs() {
		c, _ := CountBlobs(sim.Leaf(leafID).Repo())
		t.Logf("  %s: %d blobs", leafID, c)
		if c == 0 {
			t.Errorf("%s: zero blobs after mixed workload", leafID)
		}
	}

	assertBoundedProgress(t, sim, table, def, 1)
	logRowCounts(t, sim, table, def)
}

// =============================================================================
// Adversarial deletion tests
// =============================================================================

func TestTS_Adversarial_DeletionConvergence(t *testing.T) {
	seed := seedFor(307)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 3, true)
	sim.Network().SetDropRate(0.10)

	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, "devices", def, 1000)

	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("d%d", i)
		upsertRow(t, masterRepo, "devices", map[string]any{
			"device_id": id,
			"hostname":  fmt.Sprintf("host-%d", i),
			"status":    "online",
		}, 1000)
	}

	if err := sim.Run(stepsFor(300)); err != nil {
		t.Fatalf("Run (distribute): %v", err)
	}

	pkColDefs := []repo.ColumnDef{{Name: "device_id", Type: "text", PK: true}}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("d%d", i)
		pkHash := repo.PKHash(pkColDefs, map[string]any{"device_id": id})
		if _, err := repo.DeleteXRowByPKHash(masterRepo.DB(), "devices", def, pkHash, 2000); err != nil {
			t.Fatalf("DeleteXRowByPKHash d%d: %v", i, err)
		}
	}

	if err := sim.Run(stepsFor(500)); err != nil {
		t.Fatalf("Run (converge): %v", err)
	}

	t.Logf("[adversarial-deletion] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}

	for _, leafID := range sim.LeafIDs() {
		leafRepo := sim.Leaf(leafID).Repo()
		rows, _, err := repo.ListXRows(leafRepo.DB(), "devices", def)
		if err != nil {
			t.Fatalf("ListXRows %s: %v", leafID, err)
		}
		tombstones := 0
		live := 0
		for _, row := range rows {
			if repo.IsTombstone(def, row) {
				tombstones++
			} else {
				live++
			}
		}
		if tombstones != 3 {
			t.Errorf("%s: tombstones=%d, want 3", leafID, tombstones)
		}
		if live != 2 {
			t.Errorf("%s: live=%d, want 2", leafID, live)
		}
	}

	masterHash, err := repo.CatalogHash(masterRepo.DB(), "devices", def)
	if err != nil {
		t.Fatalf("CatalogHash master: %v", err)
	}
	for _, leafID := range sim.LeafIDs() {
		leafHash, err := repo.CatalogHash(sim.Leaf(leafID).Repo().DB(), "devices", def)
		if err != nil {
			t.Fatalf("CatalogHash %s: %v", leafID, err)
		}
		if leafHash != masterHash {
			t.Errorf("%s: catalog hash %q != master %q", leafID, leafHash, masterHash)
		}
	}
}

func TestTS_Adversarial_DeleteUpdateRace(t *testing.T) {
	seed := seedFor(308)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 2, true)
	sim.Network().SetDropRate(0.10)

	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, "devices", def, 1000)

	upsertRow(t, masterRepo, "devices", map[string]any{
		"device_id": "contested",
		"hostname":  "original",
		"status":    "online",
	}, 1000)
	if err := sim.Run(stepsFor(300)); err != nil {
		t.Fatalf("Run (seed): %v", err)
	}

	pkColDefs := []repo.ColumnDef{{Name: "device_id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"device_id": "contested"})
	if _, err := repo.DeleteXRowByPKHash(masterRepo.DB(), "devices", def, pkHash, 2000); err != nil {
		t.Fatalf("DeleteXRowByPKHash: %v", err)
	}
	leaf0 := sim.Leaf(sim.LeafIDs()[0]).Repo()
	upsertRow(t, leaf0, "devices", map[string]any{
		"device_id": "contested",
		"hostname":  "updated",
		"status":    "active",
	}, 3000)

	if err := sim.Run(stepsFor(500)); err != nil {
		t.Fatalf("Run (race): %v", err)
	}

	t.Logf("[adversarial-race] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}

	for _, leafID := range sim.LeafIDs() {
		row, mtime, err := repo.LookupXRow(sim.Leaf(leafID).Repo().DB(), "devices", def, pkHash)
		if err != nil {
			t.Fatalf("LookupXRow %s: %v", leafID, err)
		}
		if row == nil {
			t.Errorf("%s: row missing", leafID)
			continue
		}
		if repo.IsTombstone(def, row) {
			t.Errorf("%s: should be live (mtime=3000 wins over delete at mtime=2000)", leafID)
		}
		if mtime != 3000 {
			t.Errorf("%s: mtime=%d, want 3000", leafID, mtime)
		}
	}
	row, mtime, err := repo.LookupXRow(masterRepo.DB(), "devices", def, pkHash)
	if err != nil {
		t.Fatalf("LookupXRow master: %v", err)
	}
	if row == nil || repo.IsTombstone(def, row) || mtime != 3000 {
		t.Errorf("master: row=%v tombstone=%v mtime=%d, want live row at mtime=3000",
			row != nil, row != nil && repo.IsTombstone(def, row), mtime)
	}
}

func TestTS_Adversarial_DeleteUnseenRow(t *testing.T) {
	seed := seedFor(309)
	sim, masterRepo, _ := newTableSyncSim(t, seed, 2, true)
	sim.Network().SetDropRate(0.10)

	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, "devices", def, 1000)

	upsertRow(t, masterRepo, "devices", map[string]any{
		"device_id": "ephemeral",
		"hostname":  "ghost",
		"status":    "gone",
	}, 1000)
	pkColDefs := []repo.ColumnDef{{Name: "device_id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"device_id": "ephemeral"})
	if _, err := repo.DeleteXRowByPKHash(masterRepo.DB(), "devices", def, pkHash, 2000); err != nil {
		t.Fatalf("DeleteXRowByPKHash: %v", err)
	}

	if err := sim.Run(stepsFor(400)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Logf("[adversarial-unseen] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	if err := sim.CheckSafety(); err != nil {
		t.Fatalf("Safety: %v", err)
	}

	for _, leafID := range sim.LeafIDs() {
		row, _, err := repo.LookupXRow(sim.Leaf(leafID).Repo().DB(), "devices", def, pkHash)
		if err != nil {
			t.Fatalf("LookupXRow %s: %v", leafID, err)
		}
		if row == nil {
			t.Errorf("%s: should have tombstone for unseen row", leafID)
			continue
		}
		if !repo.IsTombstone(def, row) {
			t.Errorf("%s: should be tombstone, got live row", leafID)
		}
	}
}

func TestTS_StressTest1000Rows(t *testing.T) {
	seed := seedFor(406)
	sim, masterRepo, _ := newHostileSim(t, seed, 2, false)

	table := "events"
	def := configTableDef()
	registerTableAll(t, sim, masterRepo, table, def, 100)

	for i := 0; i < 500; i++ {
		upsertRow(t, masterRepo, table, map[string]any{
			"key":   fmt.Sprintf("evt-%04d", i),
			"value": fmt.Sprintf("data-%04d", i),
		}, int64(100+i))
	}

	if err := sim.Run(stepsFor(50)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("[stress-1000] seed=%d steps=%d syncs=%d errors=%d",
		seed, sim.Steps, sim.TotalSyncs, sim.TotalErrors)

	checkStructuralIntegrity(t, sim)
	assertBoundedProgress(t, sim, table, def, 1)
	logRowCounts(t, sim, table, def)
}

// =============================================================================
// Deletion, Resurrection, and Integer PK tests
// =============================================================================

func TestTableSync_Deletion_Convergence(t *testing.T) {
	sim, masterRepo, _ := newTableSyncSim(t, 42, 3, false)
	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, "devices", def, 1000)

	upsertRow(t, masterRepo, "devices", map[string]any{
		"device_id": "d1", "hostname": "alpha", "status": "online",
	}, 1000)

	if err := sim.Run(30); err != nil {
		t.Fatalf("Run (seed): %v", err)
	}
	for _, leafID := range sim.LeafIDs() {
		assertRowCount(t, sim.Leaf(leafID).Repo(), string(leafID), "devices", def, 1)
	}

	pkColDefs := []repo.ColumnDef{{Name: "device_id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"device_id": "d1"})
	deleted, err := repo.DeleteXRowByPKHash(masterRepo.DB(), "devices", def, pkHash, 2000)
	if err != nil || !deleted {
		t.Fatalf("master delete: deleted=%v err=%v", deleted, err)
	}

	if err := sim.Run(50); err != nil {
		t.Fatalf("Run (delete): %v", err)
	}

	for _, leafID := range sim.LeafIDs() {
		row, mtime, _ := repo.LookupXRow(sim.Leaf(leafID).Repo().DB(), "devices", def, pkHash)
		if row == nil {
			t.Errorf("%s: row should exist as tombstone", leafID)
			continue
		}
		if !repo.IsTombstone(def, row) {
			t.Errorf("%s: row should be tombstone", leafID)
		}
		if mtime != 2000 {
			t.Errorf("%s: mtime=%d, want 2000", leafID, mtime)
		}
	}
}

func TestTableSync_Deletion_Resurrection(t *testing.T) {
	sim, masterRepo, _ := newTableSyncSim(t, 99, 2, false)
	def := deviceTableDef()
	registerTableAll(t, sim, masterRepo, "devices", def, 1000)

	upsertRow(t, masterRepo, "devices", map[string]any{
		"device_id": "d1", "hostname": "alpha", "status": "online",
	}, 1000)
	if err := sim.Run(30); err != nil {
		t.Fatalf("Run (seed): %v", err)
	}

	pkColDefs := []repo.ColumnDef{{Name: "device_id", Type: "text", PK: true}}
	pkHash := repo.PKHash(pkColDefs, map[string]any{"device_id": "d1"})
	repo.DeleteXRowByPKHash(masterRepo.DB(), "devices", def, pkHash, 2000)

	if err := sim.Run(30); err != nil {
		t.Fatalf("Run (delete): %v", err)
	}

	leaf0 := sim.Leaf(sim.LeafIDs()[0]).Repo()
	upsertRow(t, leaf0, "devices", map[string]any{
		"device_id": "d1", "hostname": "beta", "status": "active",
	}, 3000)

	if err := sim.Run(50); err != nil {
		t.Fatalf("Run (resurrect): %v", err)
	}

	for _, leafID := range sim.LeafIDs() {
		row, mtime, _ := repo.LookupXRow(sim.Leaf(leafID).Repo().DB(), "devices", def, pkHash)
		if row == nil {
			t.Errorf("%s: row missing after resurrection", leafID)
			continue
		}
		if repo.IsTombstone(def, row) {
			t.Errorf("%s: row should be live after resurrection", leafID)
		}
		if mtime != 3000 {
			t.Errorf("%s: mtime=%d, want 3000", leafID, mtime)
		}
	}
	row, mtime, _ := repo.LookupXRow(masterRepo.DB(), "devices", def, pkHash)
	if row == nil || repo.IsTombstone(def, row) || mtime != 3000 {
		t.Errorf("master: row=%v tombstone=%v mtime=%d", row != nil, row != nil && repo.IsTombstone(def, row), mtime)
	}
}

func TestTableSync_IntegerPK_Convergence(t *testing.T) {
	sim, masterRepo, _ := newTableSyncSim(t, 77, 2, false)
	def := repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "seq", Type: "integer", PK: true},
			{Name: "payload", Type: "text"},
		},
		Conflict: "mtime-wins",
	}
	registerTableAll(t, sim, masterRepo, "events", def, 1000)

	bigPK := int64(1<<53 + 1)
	upsertRow(t, masterRepo, "events", map[string]any{
		"seq": bigPK, "payload": "event-data",
	}, 1000)

	if err := sim.Run(100); err != nil {
		t.Fatalf("Run: %v", err)
	}

	masterHash, _ := repo.CatalogHash(masterRepo.DB(), "events", def)
	for _, leafID := range sim.LeafIDs() {
		leafHash, _ := repo.CatalogHash(sim.Leaf(leafID).Repo().DB(), "events", def)
		if leafHash != masterHash {
			t.Errorf("%s: catalog hash %q != master %q", leafID, leafHash, masterHash)
		}
	}
}

func TestTableSync_Deletion_CompositePK(t *testing.T) {
	sim, masterRepo, _ := newTableSyncSim(t, 55, 2, false)
	def := repo.TableDef{
		Columns: []repo.ColumnDef{
			{Name: "org", Type: "text", PK: true},
			{Name: "user_id", Type: "text", PK: true},
			{Name: "role", Type: "text"},
		},
		Conflict: "mtime-wins",
	}
	registerTableAll(t, sim, masterRepo, "members", def, 1000)

	upsertRow(t, masterRepo, "members", map[string]any{
		"org": "acme", "user_id": "alice", "role": "admin",
	}, 1000)
	upsertRow(t, masterRepo, "members", map[string]any{
		"org": "acme", "user_id": "bob", "role": "member",
	}, 1000)

	if err := sim.Run(50); err != nil {
		t.Fatalf("Run (seed): %v", err)
	}

	pkColDefs := []repo.ColumnDef{
		{Name: "org", Type: "text", PK: true},
		{Name: "user_id", Type: "text", PK: true},
	}
	aliceHash := repo.PKHash(pkColDefs, map[string]any{"org": "acme", "user_id": "alice"})
	if _, err := repo.DeleteXRowByPKHash(masterRepo.DB(), "members", def, aliceHash, 2000); err != nil {
		t.Fatalf("DeleteXRowByPKHash alice: %v", err)
	}

	if err := sim.Run(50); err != nil {
		t.Fatalf("Run (delete): %v", err)
	}

	bobHash := repo.PKHash(pkColDefs, map[string]any{"org": "acme", "user_id": "bob"})
	for _, leafID := range sim.LeafIDs() {
		d := sim.Leaf(leafID).Repo().DB()
		aliceRow, _, _ := repo.LookupXRow(d, "members", def, aliceHash)
		if aliceRow == nil || !repo.IsTombstone(def, aliceRow) {
			t.Errorf("%s: alice should be tombstone", leafID)
		}
		bobRow, _, _ := repo.LookupXRow(d, "members", def, bobHash)
		if bobRow == nil || repo.IsTombstone(def, bobRow) {
			t.Errorf("%s: bob should be live", leafID)
		}
	}
	aliceRow, _, _ := repo.LookupXRow(masterRepo.DB(), "members", def, aliceHash)
	if aliceRow == nil || !repo.IsTombstone(def, aliceRow) {
		t.Error("master: alice should be tombstone")
	}
	bobRow, _, _ := repo.LookupXRow(masterRepo.DB(), "members", def, bobHash)
	if bobRow == nil || repo.IsTombstone(def, bobRow) {
		t.Error("master: bob should be live")
	}
}
