package store

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestPostgresEveryMethodExecutes calls each store method once against Postgres.
//
// Not a correctness test -- the conformance suite is where behaviour is pinned. This
// answers a narrower and, it turned out, urgent question: does the statement run at all
// on this engine?
//
// It exists because the answer was measured to be "unknown for 23 of 38 methods". The
// suite passed 8/8 while Release had never executed on Postgres, and Release used a
// SQLite-only MAX that could not have worked. A statement nothing calls is a statement no
// engine has ever parsed, and the dialect layer cannot tell the difference.
func TestPostgresEveryMethodExecutes(t *testing.T) {
	dsn := os.Getenv("BEAN_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set BEAN_TEST_POSTGRES_DSN (see hack/postgres-conformance.sh)")
	}
	s, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.DropAllForTest(); err != nil {
		t.Fatalf("reset: %v", err)
	}

	now := time.Now()
	// check runs one call and reports the method name, so a failure names the method
	// rather than a line number in a long function.
	check := func(name string, err error) {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	check("PutSandbox", s.PutSandbox(&Sandbox{ID: "sbx-1", State: SandboxRunning, CreatedAt: now}))
	_, err = s.GetSandbox("sbx-1")
	check("GetSandbox", err)
	_, err = s.ListSandboxes("", "", "")
	check("ListSandboxes", err)
	check("AppendEvent", s.AppendEvent(&Event{SandboxID: "sbx-1", Type: "x", Timestamp: now}))
	_, err = s.ListEvents("sbx-1", 10)
	check("ListEvents", err)

	check("PutTemplate", s.PutTemplate(&Template{ID: "tpl_1", Name: "img:1", State: TemplateReady, UpdatedAt: now}))
	_, err = s.GetTemplate("tpl_1")
	check("GetTemplate", err)
	_, err = s.GetTemplateByName("img:1")
	check("GetTemplateByName", err)
	_, err = s.ListTemplates("")
	check("ListTemplates", err)

	check("PutBuild", s.PutBuild(&ImageBuild{ID: "b-1", State: BuildPending, CreatedAt: now}))
	_, err = s.GetBuild("b-1")
	check("GetBuild", err)
	_, err = s.ListBuilds("")
	check("ListBuilds", err)

	check("PutPrewarmJob", s.PutPrewarmJob(&PrewarmJob{ID: "pw-1", CreatedAt: now}))
	_, err = s.GetPrewarmJob("pw-1")
	check("GetPrewarmJob", err)

	check("PutSnapshot", s.PutSnapshot(&Snapshot{ID: "sn-1", SandboxID: "sbx-1", State: SnapshotReady}))
	check("PutSnapshot/child", s.PutSnapshot(&Snapshot{ID: "sn-2", SandboxID: "sbx-1", State: SnapshotReady, BaseID: "sn-1"}))
	_, err = s.ListSnapshots("", "", "")
	check("ListSnapshots", err)
	_, err = s.SnapshotChain("sn-2")
	check("SnapshotChain", err)
	_, err = s.GetSnapshot("sn-1")
	check("GetSnapshot", err)
	// Acquired then released so the child delete below is not refused. These three are
	// pinned by the conformance suite as well; called here because the drift guard is
	// about which statements this engine has parsed, not about who checks the behaviour.
	_, err = s.AcquireSnapshot("sn-1")
	check("AcquireSnapshot", err)
	check("ReleaseSnapshot", s.ReleaseSnapshot("sn-1"))

	check("UpsertNode", s.UpsertNode(&NodeRecord{
		ID: "n-1", Region: "r", State: "READY", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 32768,
		LastHeartbeat: now,
	}))
	_, err = s.LoadNodes()
	check("LoadNodes", err)
	_, err = s.GetNode("n-1")
	check("GetNode", err)
	check("RenewLease", s.RenewLease("n-1"))
	check("SetNodeDiskUsed", s.SetNodeDiskUsed("n-1", 128))
	check("PutNodeImages", s.PutNodeImages("n-1", map[string]CachedImage{"img:1": {SizeBytes: 1}}))
	_, err = s.StaleNodes(now.Add(-time.Minute))
	check("StaleNodes", err)
	_, err = s.SetNodeState("n-1", "READY")
	check("SetNodeState", err)

	check("Reserve", s.Reserve("n-1", &Reservation{SandboxID: "sbx-1", CPU: 1, MemoryMiB: 512, DiskMiB: 1024, SpreadKey: "g"}))
	_, err = s.SpreadCounts("g")
	check("SpreadCounts", err)
	_, err = s.OrphanReservations()
	check("OrphanReservations", err)
	check("FinishCreate", s.FinishCreate("n-1"))
	check("Release", s.Release("sbx-1"))

	check("DeleteSnapshot/child", s.DeleteSnapshot("sn-2"))
	check("DeleteSnapshot", s.DeleteSnapshot("sn-1"))
	check("DeleteTemplate", s.DeleteTemplate("tpl_1"))
	check("DeleteSandbox", s.DeleteSandbox("sbx-1"))
}

// TestEveryInterfaceMethodIsExercisedOnPostgres keeps the smoke test from drifting.
//
// The list of calls above is hand-written, so it decays the same way the interfaces do: a
// method added later is a statement no engine has parsed, and the symptom is a runtime
// failure in production on whichever engine the author was not running.
//
// Checked by reflection against the interfaces rather than against a second hand-written
// list, because a hardcoded list of expected names would need the same maintenance it is
// meant to enforce.
func TestEveryInterfaceMethodIsExercisedOnPostgres(t *testing.T) {
	source, err := os.ReadFile("postgres_smoke_test.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)

	// Every method on the union of the store's interfaces, read off the types rather
	// than listed.
	var union = []reflect.Type{
		reflect.TypeOf((*Sandboxes)(nil)).Elem(),
		reflect.TypeOf((*Snapshots)(nil)).Elem(),
		reflect.TypeOf((*Templates)(nil)).Elem(),
		reflect.TypeOf((*Placement)(nil)).Elem(),
		reflect.TypeOf((*Nodes)(nil)).Elem(),
		reflect.TypeOf((*RegistryCredentials)(nil)).Elem(),
		reflect.TypeOf((*Builds)(nil)).Elem(),
	}

	seen := map[string]bool{}
	missing := []string{}
	for _, iface := range union {
		for i := 0; i < iface.NumMethod(); i++ {
			name := iface.Method(i).Name
			if seen[name] {
				continue
			}
			seen[name] = true
			// Credentials are covered by the conformance suite's own requirement, so
			// they are not repeated here.
			if strings.HasSuffix(name, "RegistryCredential") ||
				name == "ListRegistryCredentials" {
				continue
			}
			if !strings.Contains(body, "s."+name+"(") {
				missing = append(missing, name)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these methods are never executed against Postgres: %v\n"+
			"A statement no test calls is a statement this engine has never parsed. That "+
			"is not theoretical: Release used SQLite's two-argument MAX, which Postgres "+
			"cannot run, and the conformance suite passed 8/8 without ever calling it.",
			missing)
	}
	if len(seen) == 0 {
		t.Fatal("no interface methods found, so this test proves nothing")
	}
}
