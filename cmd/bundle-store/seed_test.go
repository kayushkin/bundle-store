package main

import (
	"testing"

	bundlestore "github.com/kayushkin/bundle-store"
)

// seedBundles runs on every boot. Its doc comment makes a promise that nothing
// tested: "existing rows preserve the operator's enabled state, so hand-curation
// in the UI survives restarts."
//
// That promise is what makes POST /bundles/{id}/disable mean anything for a
// seeded bundle. UpsertBundle writes `enabled` unconditionally from the struct
// it is handed, so the preservation lives entirely in the two lines above the
// call — a GetBundleByName followed by copying the stored flag onto the seed.
// Drop those two lines and the service still boots, still seeds, still passes
// every other test, and silently re-enables every bundle the operator turned
// off, once per restart.

// fullIndex resolves every member the seed list uses. The ids are arbitrary but
// distinct, so a member landing under the wrong name is visible — except
// browser-automation's 1446, which the react bundle pins by id.
func fullIndex() *memberIndex {
	return indexFrom(
		[]storeEntry{
			{ID: 1446, Name: "browser-automation"},
			{ID: 298, Name: "senior-frontend"},
			{ID: 320, Name: "cc-skill-backend-patterns"},
			{ID: 321, Name: "sql-pro"},
		},
		[]storeEntry{
			{ID: 13, Name: "playwright"},
			{ID: 14, Name: "chrome-devtools"},
		},
	)
}

func openSeeded(t *testing.T) *bundlestore.Store {
	t.Helper()
	store, err := bundlestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := seedBundles(store, fullIndex()); err != nil {
		t.Fatalf("first seed: %v", err)
	}
	return store
}

func TestSeedingTwiceIsStable(t *testing.T) {
	store := openSeeded(t)
	before, err := store.ListBundles(bundlestore.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("the seed wrote no bundles")
	}
	if err := seedBundles(store, fullIndex()); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	after, err := store.ListBundles(bundlestore.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("re-seeding changed the bundle count: %d -> %d", len(before), len(after))
	}
}

// The claim in the doc comment, stated as a test. A restart is a second call to
// seedBundles against a store that already holds the rows.
func TestARestartPreservesABundleTheOperatorDisabled(t *testing.T) {
	store := openSeeded(t)

	react, err := store.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if !react.Enabled {
		t.Fatal("fixture is broken: the seed ships react enabled")
	}
	// The operator turns it off through the API.
	if err := store.SetEnabled(react.ID, false); err != nil {
		t.Fatal(err)
	}

	// The service restarts.
	if err := seedBundles(store, fullIndex()); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	got, err := store.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("the restart re-enabled a bundle the operator had disabled. UpsertBundle writes " +
			"`enabled` from the seed struct, so seedBundles must read the stored flag back onto " +
			"the seed before upserting — see the GetBundleByName call in seedBundles.")
	}
	if got.ID != react.ID {
		t.Fatalf("the re-seed replaced the row rather than updating it: id %d -> %d", react.ID, got.ID)
	}
}

// The other half of the same promise, and the reason it cannot be implemented
// by simply skipping bundles that already exist: definitions must still refresh
// from the seed file. A seedBundles that preserved `enabled` by not writing at
// all would pass the test above and freeze every curated definition at whatever
// the first boot wrote.
func TestARestartStillRefreshesTheDefinitionOfADisabledBundle(t *testing.T) {
	store := openSeeded(t)

	react, err := store.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(react.ID, false); err != nil {
		t.Fatal(err)
	}
	// Something edited the row's definition out from under the seed file.
	react.DisplayName = "Stale hand edit"
	react.Members = nil
	react.Enabled = false
	if _, err := store.UpsertBundle(react); err != nil {
		t.Fatal(err)
	}

	if err := seedBundles(store, fullIndex()); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	got, err := store.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName == "Stale hand edit" {
		t.Fatal("the seed did not refresh the display name — preserving `enabled` must not mean " +
			"skipping the row")
	}
	if len(got.Members) == 0 {
		t.Fatal("the seed did not restore the member set")
	}
	if got.Enabled {
		t.Fatal("refreshing the definition re-enabled the bundle")
	}
}

// A bundle the operator enabled that ships disabled must survive too. The seed
// list currently ships everything enabled, so this direction is only reachable
// by disabling first and enabling back — which is what an operator does, and
// what a preservation implemented as "force false on existing rows" would break.
func TestARestartPreservesABundleTheOperatorEnabledAgain(t *testing.T) {
	store := openSeeded(t)

	base, err := store.GetBundleByName("base")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(base.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnabled(base.ID, true); err != nil {
		t.Fatal(err)
	}

	if err := seedBundles(store, fullIndex()); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	got, err := store.GetBundleByName("base")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled {
		t.Fatal("the restart disabled a bundle the operator had enabled")
	}
}
