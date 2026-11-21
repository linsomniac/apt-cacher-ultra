package freshness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linsomniac/apt-cacher-ultra/internal/cache"
)

// rigPackages is a Packages index of n packages, pNN version 1, each with
// a hash of its own: Filename → SHA256.
func rigPackages(n int) map[string]string {
	pkgs := map[string]string{}
	for i := 0; i < n; i++ {
		pkgs[fmt.Sprintf("pool/main/p/p%02d/p%02d_1.deb", i, i)] = fmt.Sprintf("%064x", 1000+i)
	}
	return pkgs
}

// publishPackages serves pkgs as the rig suite's Packages index and adopts
// an InRelease labelled label that declares it.
func (r *retryRig) publishPackages(label string, pkgs map[string]string) error {
	r.t.Helper()
	r.pkgs = fakePackagesStanzas(pkgs)
	r.members.put(rigPkgsURL, r.pkgs)
	return r.adopter.Run(context.Background(), SuiteRef{"http", rigHost, rigSuite}, r.release(label, 0), "", "")
}

// rigPackageList returns the id of the package list the rig suite names.
func rigPackageList(t *testing.T, r *retryRig) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(r.cache.Dir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var id sql.NullInt64
	if err := db.QueryRow(`SELECT package_list_id FROM suite_freshness WHERE canonical_host = ? AND suite_path = ?`,
		rigHost, rigSuite).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if !id.Valid {
		t.Fatal("the rig suite names no package list")
	}
	return id.Int64
}

// lastFinished returns the last adoption_finished record in logs.
func lastFinished(t *testing.T, logs *lockedBuffer) map[string]any {
	t.Helper()
	recs := logRecords(t, logs, "adoption_finished")
	if len(recs) == 0 {
		t.Fatal("no adoption_finished record")
	}
	return recs[len(recs)-1]
}

// A publication that changed one package writes that package's rows into
// the suite's list, not the list: the finish record says so, and the list
// is the one the first adoption wrote.
func TestAdoption_AChangedPublicationWritesOnlyTheDifference(t *testing.T) {
	r := newRetryRig(t, 0)
	pkgs := rigPackages(40)
	if err := r.publishPackages("first", pkgs); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	list := rigPackageList(t, r)
	logs := adoptionLogs(r)

	// p07 goes from version 1 to 2: one file leaves the index, one enters.
	delete(pkgs, "pool/main/p/p07/p07_1.deb")
	pkgs["pool/main/p/p07/p07_2.deb"] = fmt.Sprintf("%064x", 7777)
	if err := r.publishPackages("second", pkgs); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	rec := lastFinished(t, logs)
	for field, want := range map[string]any{
		"outcome": "success", "package_list_write": "difference",
		"package_rows_added": float64(1), "package_rows_changed": float64(0), "package_rows_removed": float64(1),
	} {
		if rec[field] != want {
			t.Errorf("adoption_finished %s = %v, want %v", field, rec[field], want)
		}
	}
	if _, has := rec["package_list_rewrite_reason"]; has {
		t.Errorf("a difference carries package_list_rewrite_reason = %v", rec["package_list_rewrite_reason"])
	}
	if got := rigPackageList(t, r); got != list {
		t.Errorf("the suite names list %d, want the first adoption's %d: a difference is written in place", got, list)
	}
	// The cache vouches the new file and no longer the old one.
	for path, want := range map[string]int{"/r/pool/main/p/p07/p07_2.deb": 1, "/r/pool/main/p/p07/p07_1.deb": 0, "/r/pool/main/p/p08/p08_1.deb": 1} {
		rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", rigHost, path)
		if err != nil || len(rows) != want {
			t.Errorf("DeclaredHashesForPath(%s) = %v, %v; want %d rows", path, rows, err, want)
		}
	}
}

func TestAdoption_TheFirstAdoptionReportsARewrite(t *testing.T) {
	r := newRetryRig(t, 0)
	logs := adoptionLogs(r)
	if err := r.publishPackages("first", rigPackages(40)); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	rec := lastFinished(t, logs)
	for field, want := range map[string]any{
		"package_list_write": "rewrite", "package_list_rewrite_reason": "first",
		"package_rows_added": float64(40), "package_rows_changed": float64(0), "package_rows_removed": float64(0),
	} {
		if rec[field] != want {
			t.Errorf("adoption_finished %s = %v, want %v", field, rec[field], want)
		}
	}
}

// The difference is an optimisation: when the live list cannot be read the
// adoption goes ahead and writes the whole list.
func TestAdoption_AFailedListReadStillAdopts(t *testing.T) {
	r := newRetryRig(t, 0)
	pkgs := rigPackages(40)
	if err := r.publishPackages("first", pkgs); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	logs := adoptionLogs(r)
	r.adopter.packageListDiff = func(context.Context, cache.SuiteKey, []cache.PackageHash) (*cache.PackageListDiff, error) {
		return nil, errors.New("the list could not be read")
	}
	pkgs["pool/main/p/new/new_1.deb"] = fmt.Sprintf("%064x", 4242)
	if err := r.publishPackages("second", pkgs); err != nil {
		t.Fatalf("an adoption whose list read failed: %v", err)
	}
	warns := logRecords(t, logs, "package_list_diff_failed")
	if len(warns) != 1 || warns[0]["level"] != "WARN" || warns[0]["canonical_host"] != rigHost || warns[0]["suite_path"] != rigSuite ||
		warns[0]["err"] != "the list could not be read" {
		t.Fatalf("package_list_diff_failed records = %v, want one WARN naming the suite and the error", warns)
	}
	rec := lastFinished(t, logs)
	if rec["outcome"] != "success" || rec["package_list_write"] != "rewrite" || rec["package_list_rewrite_reason"] != "no_difference" ||
		rec["package_rows_added"] != float64(41) {
		t.Fatalf("adoption_finished = %v; want a success that rewrote 41 rows for no_difference", rec)
	}
	rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", rigHost, "/r/pool/main/p/new/new_1.deb")
	if err != nil || len(rows) != 1 {
		t.Fatalf("the new package is not vouched: %v, %v", rows, err)
	}
}

// A difference that does not produce the publication's rows is a defect
// worth an ERROR, and costs nothing else: the commit writes the whole
// list, the adoption succeeds, and the cache vouches the new rows.
func TestAdoption_AMismatchIsLoggedAsAnError(t *testing.T) {
	r := newRetryRig(t, 0)
	pkgs := rigPackages(40)
	if err := r.publishPackages("first", pkgs); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	logs := adoptionLogs(r)
	r.adopter.packageListDiff = func(ctx context.Context, key cache.SuiteKey, rows []cache.PackageHash) (*cache.PackageListDiff, error) {
		diff, err := r.cache.DiffPackageList(ctx, key, rows)
		if err != nil || diff == nil || len(diff.Removed) != 2 {
			t.Errorf("the real difference = %+v, %v; want two removed paths", diff, err)
			return diff, err
		}
		diff.Removed = diff.Removed[:1] // the defect
		return diff, nil
	}
	delete(pkgs, "pool/main/p/p01/p01_1.deb")
	delete(pkgs, "pool/main/p/p02/p02_1.deb")
	pkgs["pool/main/p/p03/p03_1.deb"] = fmt.Sprintf("%064x", 3333) // a changed hash
	if err := r.publishPackages("second", pkgs); err != nil {
		t.Fatalf("an adoption with a wrong difference: %v", err)
	}
	errs := logRecords(t, logs, "package_list_mismatch")
	if len(errs) != 1 {
		t.Fatalf("package_list_mismatch records = %v, want one", errs)
	}
	e := errs[0]
	if e["level"] != "ERROR" || e["canonical_host"] != rigHost || e["suite_path"] != rigSuite ||
		e["expected_rows"] != float64(38) || e["found_rows"] != float64(39) ||
		e["added"] != float64(0) || e["changed"] != float64(1) || e["removed"] != float64(1) || e["detail"] == nil || e["snapshot_id"] == nil {
		t.Fatalf("package_list_mismatch = %v; want an ERROR for the suite: expected 38 rows, found 39, the difference's sizes and a detail", e)
	}
	rec := lastFinished(t, logs)
	if rec["outcome"] != "success" || rec["package_list_write"] != "rewrite" || rec["package_list_rewrite_reason"] != "mismatch" ||
		rec["package_rows_added"] != float64(38) {
		t.Fatalf("adoption_finished = %v; want a success that rewrote 38 rows for mismatch", rec)
	}
	rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", rigHost, "/r/pool/main/p/p03/p03_1.deb")
	if err != nil || len(rows) != 1 || rows[0].DeclaredSHA256 != fmt.Sprintf("%064x", 3333) {
		t.Fatalf("the changed package's vouched hash = %v, %v; want the new one", rows, err)
	}
	for _, gone := range []string{"/r/pool/main/p/p01/p01_1.deb", "/r/pool/main/p/p02/p02_1.deb"} {
		if rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", rigHost, gone); err != nil || len(rows) != 0 {
			t.Fatalf("%s is still vouched: %v, %v", gone, rows, err)
		}
	}
}

// An attempt that did not commit says nothing about a package list.
func TestAdoption_AFailedAttemptCarriesNoPackageListFields(t *testing.T) {
	r := newRetryRig(t, 0)
	if err := r.publishPackages("first", rigPackages(5)); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	logs := adoptionLogs(r)
	rejected := r.release("rejected", 0)
	r.ver.reject(contentKey(adoptionFormInline, rejected, nil, nil))
	if err := r.adopter.Run(context.Background(), SuiteRef{"http", rigHost, rigSuite}, rejected, "", ""); err == nil {
		t.Fatal("an adoption of a rejected InRelease succeeded")
	}
	rec := lastFinished(t, logs)
	if rec["outcome"] == "success" {
		t.Fatalf("adoption_finished = %v, want a failure", rec)
	}
	for _, field := range []string{"package_list_write", "package_list_rewrite_reason", "package_rows_added", "package_rows_changed", "package_rows_removed"} {
		if v, has := rec[field]; has {
			t.Errorf("a failed attempt carries %s = %v", field, v)
		}
	}
}

// An unsigned adoption has no package rows: no difference is read for it
// and its finish record says the suite names no list.
func TestAdoption_AnUnsignedAdoptionReadsNoDifference(t *testing.T) {
	r := newRetryRig(t, 0)
	logs := adoptionLogs(r)
	r.adopter.packageListDiff = func(context.Context, cache.SuiteKey, []cache.PackageHash) (*cache.PackageListDiff, error) {
		t.Error("the live package list was read for an unsigned adoption")
		return nil, nil
	}
	r.adoptUnsigned("plain")
	rec := lastFinished(t, logs)
	if rec["outcome"] != "success" || rec["package_list_write"] != "none" {
		t.Fatalf("adoption_finished = %v; want a success whose package_list_write is none", rec)
	}
}

// adoption.write_package_list_differences = false is the way to switch the
// difference off without a new release: the adopter neither reads the live
// list nor hands the commit a difference, so every adoption writes the
// suite's whole list as a new one, as releases before schema 11 did. The
// finish record says the difference was switched off, not that a read
// failed; a suite's first list is "first" as ever.
func TestAdoption_WithDifferencesSwitchedOffEveryAdoptionWritesTheWholeList(t *testing.T) {
	r := newRetryRig(t, 0)
	r.adopter.wholePackageLists = true
	r.adopter.packageListDiff = func(context.Context, cache.SuiteKey, []cache.PackageHash) (*cache.PackageListDiff, error) {
		t.Error("the live package list was read with differences switched off")
		return nil, nil
	}
	logs := adoptionLogs(r)
	pkgs := rigPackages(40)
	if err := r.publishPackages("first", pkgs); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	if rec := lastFinished(t, logs); rec["package_list_write"] != "rewrite" || rec["package_list_rewrite_reason"] != "first" {
		t.Fatalf("the first adoption's adoption_finished = %v; want a rewrite for first", rec)
	}
	list := rigPackageList(t, r)

	// The publication TestAdoption_AChangedPublicationWritesOnlyTheDifference
	// writes as two rows.
	delete(pkgs, "pool/main/p/p07/p07_1.deb")
	pkgs["pool/main/p/p07/p07_2.deb"] = fmt.Sprintf("%064x", 7777)
	if err := r.publishPackages("second", pkgs); err != nil {
		t.Fatalf("second adoption: %v", err)
	}
	rec := lastFinished(t, logs)
	for field, want := range map[string]any{
		"outcome": "success", "package_list_write": "rewrite", "package_list_rewrite_reason": "disabled",
		"package_rows_added": float64(40), "package_rows_changed": float64(0), "package_rows_removed": float64(0),
	} {
		if rec[field] != want {
			t.Errorf("adoption_finished %s = %v, want %v", field, rec[field], want)
		}
	}
	if got := rigPackageList(t, r); got == list {
		t.Errorf("the suite still names list %d: a whole list is written as a new one", got)
	}
	for _, event := range []string{"package_list_diff_failed", "package_list_mismatch"} {
		if recs := logRecords(t, logs, event); len(recs) != 0 {
			t.Errorf("%s was logged for a difference that was switched off: %v", event, recs)
		}
	}
	for path, want := range map[string]int{"/r/pool/main/p/p07/p07_2.deb": 1, "/r/pool/main/p/p07/p07_1.deb": 0, "/r/pool/main/p/p08/p08_1.deb": 1} {
		rows, err := r.cache.DeclaredHashesForPath(context.Background(), "http", rigHost, path)
		if err != nil || len(rows) != want {
			t.Errorf("DeclaredHashesForPath(%s) = %v, %v; want %d rows", path, rows, err, want)
		}
	}
}

// A shutdown while the live list is being read abandons the adoption
// before its commit, as a shutdown anywhere before the commit does: the
// previous snapshot stays current, and the interrupted read is not
// reported as a read failure (which would send the adoption on to write
// the whole list during a stop).
func TestAdoption_AShutdownDuringTheListReadAbandonsTheAdoption(t *testing.T) {
	r := newRetryRig(t, 0)
	pkgs := rigPackages(10)
	if err := r.publishPackages("first", pkgs); err != nil {
		t.Fatalf("first adoption: %v", err)
	}
	before := r.currentSnapshot()
	logs := adoptionLogs(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := 0
	r.adopter.packageListDiff = func(context.Context, cache.SuiteKey, []cache.PackageHash) (*cache.PackageListDiff, error) {
		read++
		cancel() // the stop arrives while the list is being read
		return nil, context.Canceled
	}
	pkgs["pool/main/p/new/new_1.deb"] = fmt.Sprintf("%064x", 4242)
	r.pkgs = fakePackagesStanzas(pkgs)
	r.members.put(rigPkgsURL, r.pkgs)
	err := r.adopter.Run(ctx, SuiteRef{"http", rigHost, rigSuite}, r.release("second", 0), "", "")
	if err == nil || !strings.Contains(err.Error(), "shutdown before the commit") {
		t.Fatalf("Run = %v, want the adoption abandoned at the shutdown before the commit", err)
	}
	if read != 1 {
		t.Fatalf("the live list was read %d times, want once", read)
	}
	if after := r.currentSnapshot(); after == nil || before == nil || after.SnapshotID != before.SnapshotID {
		t.Fatalf("the current snapshot changed from %+v to %+v: an adoption abandoned at a shutdown committed", before, after)
	}
	if warns := logRecords(t, logs, "package_list_diff_failed"); len(warns) != 0 {
		t.Fatalf("a read interrupted by a shutdown was logged as a failed read: %v", warns)
	}
	rec := lastFinished(t, logs)
	if rec["outcome"] == "success" || rec["package_list_write"] != nil {
		t.Fatalf("adoption_finished = %v; want an attempt that did not commit", rec)
	}
}
