package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bundlestore "github.com/kayushkin/bundle-store"
)

func indexFrom(skills, tools []storeEntry) *memberIndex {
	return &memberIndex{byKind: map[bundlestore.MemberKind]map[string][]storeEntry{
		bundlestore.MemberSkill: groupByName(skills),
		bundlestore.MemberTool:  groupByName(tools),
	}}
}

// The example names here are live-measured, 2026-08-29: skill-store carries
// exactly one "senior-frontend" (id 1129) and tool-store exactly one
// "playwright" (id 13).
//
// ⚠️ This case used to make its point with "browser-automation" id 297, which
// stopped being a unique name in skill-store on 2026-08-04 when a second row
// (id 1446, source "host") was ingested. The fixture went on asserting the name
// was unique for 25 days while the real store had two rows and the boot seed
// could no longer resolve it. A fixture derived from the far service and never
// re-checked is a claim with an expiry date on it — see
// TestSeedRefusesASeededNameTheOwningStoreCarriesTwice below.
func TestLookupResolvesAUniqueName(t *testing.T) {
	ix := indexFrom(
		[]storeEntry{{ID: 1129, Name: "senior-frontend", Source: "antigravity"}},
		[]storeEntry{{ID: 13, Name: "playwright"}},
	)
	id, err := ix.lookup(bundlestore.MemberSkill, "senior-frontend")
	if err != nil || id != 1129 {
		t.Fatalf("got id=%d err=%v, want 1129", id, err)
	}
	id, err = ix.lookup(bundlestore.MemberTool, "playwright")
	if err != nil || id != 13 {
		t.Fatalf("got id=%d err=%v, want 13", id, err)
	}
}

// The whole point: an ambiguous name must stop the seed rather than pick a row.
// skill-store carries claude-api from both anthropics and antigravity.
func TestLookupRefusesAnAmbiguousName(t *testing.T) {
	ix := indexFrom([]storeEntry{
		{ID: 4, Name: "claude-api", Source: "anthropics"},
		{ID: 341, Name: "claude-api", Source: "antigravity"},
	}, nil)
	_, err := ix.lookup(bundlestore.MemberSkill, "claude-api")
	if err == nil {
		t.Fatal("an ambiguous name must not resolve to a winner")
	}
	for _, want := range []string{"id=4", "id=341", "anthropics", "antigravity"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must name every candidate so a curator can pick one; missing %q in: %v", want, err)
		}
	}
}

func TestLookupRefusesAMissingName(t *testing.T) {
	ix := indexFrom(nil, nil)
	if _, err := ix.lookup(bundlestore.MemberSkill, "ghost"); err == nil {
		t.Fatal("a name no store carries must be an error, not id 0")
	}
}

// A skill and a tool that share a name are different members; the index must
// not let one answer for the other.
func TestLookupKeepsTheTwoStoresApart(t *testing.T) {
	ix := indexFrom(
		[]storeEntry{{ID: 900, Name: "shared"}},
		[]storeEntry{{ID: 5, Name: "shared"}},
	)
	skillID, err := ix.lookup(bundlestore.MemberSkill, "shared")
	if err != nil {
		t.Fatal(err)
	}
	toolID, err := ix.lookup(bundlestore.MemberTool, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if skillID != 900 || toolID != 5 {
		t.Fatalf("kinds crossed: skill=%d tool=%d", skillID, toolID)
	}
}

func TestFetchEntriesReportsAnUnreachableStoreLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := fetchEntries(context.Background(), srv.URL+"/skills"); err == nil {
		t.Fatal("a non-200 from an owning store must be an error, not an empty index")
	}
}

func TestSeedFailsWhenAMemberNameCannotBeResolved(t *testing.T) {
	store, err := bundlestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// An index that knows nothing: every seeded member name is unresolvable.
	err = seedBundles(store, indexFrom(nil, nil))
	if err == nil {
		t.Fatal("seeding must fail rather than write bundles whose members point at nothing")
	}
	// And it must fail *at the lookup*, naming the store that came up empty.
	// Letting the lookup error through and tripping the store's own id check
	// instead reports the seed author as the culprit rather than the registry.
	// The first seeded member pins an id, so the lookup that fails first is that
	// id's confirmation — the same store coming up empty.
	if !strings.Contains(err.Error(), "skill-store has no skill") {
		t.Fatalf("seed must report the failed lookup, got: %v", err)
	}
}

// resolvingIndex is the "everything resolves" case: every member the starter
// bundles use maps to exactly one row. The ids are arbitrary but distinct, so a
// member landing under the wrong name is visible — except browser-automation's
// 1446, which the react bundle pins and so has to be the id here.
//
// It is NOT a model of skill-store or tool-store as they stand. Do not read an
// id here as a fact about the far store — for that, run the binary with
// -check-members against the running services.
func resolvingIndex() *memberIndex {
	return indexFrom(
		[]storeEntry{
			{ID: 1446, Name: "browser-automation"},
			{ID: 1129, Name: "senior-frontend"},
			{ID: 320, Name: "cc-skill-backend-patterns"},
			{ID: 1219, Name: "sql-pro"},
		},
		[]storeEntry{
			{ID: 13, Name: "playwright"},
			{ID: 14, Name: "chrome-devtools"},
		},
	)
}

// A seeded member name is resolved by the store that owns it, and that store can
// stop agreeing the name is unique at any time — bundle-store finds out at the
// next boot, because the seed runs there.
//
// This is not hypothetical. Measured 2026-08-29 against the running skill-store:
// "browser-automation" had TWO rows, id 297 from source "antigravity" and id
// 1446 from source "host", the second ingested 2026-08-04. The react bundle
// named that skill, so bundle-store built from main exited 1 at boot. The react
// bundle now pins id 1446 (TestSeedPinsTheHostBrowserAutomationSkillBesideItsUpstreamTwin),
// so this case puts the same collision on a member still seeded by name.
//
// Until this case existed the ambiguous half of that gate was untested through
// seedBundles: TestLookupRefusesAnAmbiguousName exercises lookup directly, and
// TestSeedFailsWhenAMemberNameCannotBeResolved covers only the MISSING half. So
// the one failure mode that actually happened was the one no test reached.
func TestSeedRefusesASeededNameTheOwningStoreCarriesTwice(t *testing.T) {
	store, err := bundlestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ix := indexFrom(
		[]storeEntry{
			{ID: 1446, Name: "browser-automation", Source: "host"},
			{ID: 1129, Name: "senior-frontend"},
			{ID: 320, Name: "cc-skill-backend-patterns"},
			{ID: 1219, Name: "sql-pro", Source: "antigravity"},
			{ID: 1500, Name: "sql-pro", Source: "host"},
		},
		[]storeEntry{
			{ID: 13, Name: "playwright"},
			{ID: 14, Name: "chrome-devtools"},
		},
	)

	err = seedBundles(store, ix)
	if err == nil {
		t.Fatal("an ambiguous member name must stop the seed rather than pick a row: " +
			"picking means a bundle silently installs whichever upstream was synced first")
	}
	// The message has to carry enough for a curator to act without reading the
	// source: which bundle, which name, and every candidate with its source.
	for _, want := range []string{"go-service", "sql-pro", "id=1219", "id=1500", "antigravity", "host"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("seed failure must name %q so a curator can pick an id; got: %v", want, err)
		}
	}

	// And it must stop BEFORE writing the bundle it could not resolve. A
	// half-seeded bundle is worse than none: /resolve would serve it.
	if _, err := store.GetBundleByName("go-service"); err == nil {
		t.Fatal("the seed wrote the go-service bundle despite failing to resolve one of its members")
	}
}

// The react bundle means the locally installed browser-automation skill — id
// 1446, ingested from ~/.claude/skills — and not the upstream antigravity copy, id
// 297, that shares its name. The owner chose it on 2026-09-10. Pinning the id is
// what lets the seed go through while skill-store carries both rows, as it has
// since 2026-08-04, without the seed choosing between them.
func TestSeedPinsTheHostBrowserAutomationSkillBesideItsUpstreamTwin(t *testing.T) {
	store, err := bundlestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ix := indexFrom(
		[]storeEntry{
			{ID: 297, Name: "browser-automation", Source: "antigravity"},
			{ID: 1446, Name: "browser-automation", Source: "host"},
			{ID: 1129, Name: "senior-frontend"},
			{ID: 320, Name: "cc-skill-backend-patterns"},
			{ID: 1219, Name: "sql-pro"},
		},
		[]storeEntry{
			{ID: 13, Name: "playwright"},
			{ID: 14, Name: "chrome-devtools"},
		},
	)
	if err := seedBundles(store, ix); err != nil {
		t.Fatalf("the seed must go through when the one ambiguous name is pinned by id: %v", err)
	}

	react, err := store.GetBundleByName("react")
	if err != nil {
		t.Fatal(err)
	}
	skillIDs := map[int64]bool{}
	for _, m := range react.Members {
		if m.Kind == bundlestore.MemberSkill {
			skillIDs[m.ID] = true
		}
	}
	if !skillIDs[1446] || skillIDs[297] {
		t.Fatalf("react must carry the host skill 1446 and not the upstream 297; skill ids: %v", skillIDs)
	}
}

// A pinned id is still checked against the store: an id the store no longer
// carries must stop the seed, not write a member that points at nothing.
func TestSeedRefusesAPinnedIDTheOwningStoreNoLongerCarries(t *testing.T) {
	store, err := bundlestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ix := indexFrom(
		[]storeEntry{
			{ID: 297, Name: "browser-automation", Source: "antigravity"},
			{ID: 1129, Name: "senior-frontend"},
			{ID: 320, Name: "cc-skill-backend-patterns"},
			{ID: 1219, Name: "sql-pro"},
		},
		[]storeEntry{
			{ID: 13, Name: "playwright"},
			{ID: 14, Name: "chrome-devtools"},
		},
	)
	err = seedBundles(store, ix)
	if err == nil {
		t.Fatal("a pinned id the owning store does not carry must stop the seed")
	}
	for _, want := range []string{"react", "id 1446", "browser-automation"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("seed failure must name %q; got: %v", want, err)
		}
	}
}

// And the pinned id must still carry the name written beside it. An id that
// now names a different skill — a typo in the id, or a rename upstream — must
// not seed that skill without a word.
func TestSeedRefusesAPinnedIDThatNamesADifferentMember(t *testing.T) {
	store, err := bundlestore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ix := indexFrom(
		[]storeEntry{
			{ID: 1446, Name: "webapp-testing", Source: "host"},
			{ID: 1129, Name: "senior-frontend"},
			{ID: 320, Name: "cc-skill-backend-patterns"},
			{ID: 1219, Name: "sql-pro"},
		},
		[]storeEntry{
			{ID: 13, Name: "playwright"},
			{ID: 14, Name: "chrome-devtools"},
		},
	)
	err = seedBundles(store, ix)
	if err == nil {
		t.Fatal("a pinned id that names another member must stop the seed")
	}
	for _, want := range []string{"react", "id 1446", "webapp-testing", "browser-automation"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("seed failure must name %q; got: %v", want, err)
		}
	}
}

// checkStarterMembers is what deploy.sh runs before it stops the running
// service, so it has to report EVERY unresolvable member, not just the first —
// the seed stops at the first, which is how a deploy fixes one name and
// discovers the next one only at the following restart.
func TestCheckStarterMembersReportsEveryUnresolvableName(t *testing.T) {
	// Nothing resolves: react names four members, go-service two.
	problems := checkStarterMembers(indexFrom(nil, nil))
	if len(problems) != 6 {
		t.Fatalf("want one line per unresolvable member across every starter bundle, got %d: %v",
			len(problems), problems)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"react", "go-service", "browser-automation", "sql-pro", "playwright"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the report must name %q; got:\n%s", want, joined)
		}
	}

	// The green case has to be able to say nothing, or the check cries wolf on
	// every deploy and gets switched off.
	if problems := checkStarterMembers(resolvingIndex()); len(problems) != 0 {
		t.Fatalf("an index where every name resolves must report nothing, got: %v", problems)
	}
}

// A denied tool is checked the same way as an included one. A deny whose name
// the owning store does not carry would deny nothing, and a session would keep
// the tool the curator meant to take away.
func TestCheckMembersCoversDeniedTools(t *testing.T) {
	bundles := []bundlestore.Bundle{{
		Name: "no-writes",
		Members: []bundlestore.Member{
			{Kind: bundlestore.MemberTool, Name: "playwright"},
			{Kind: bundlestore.MemberTool, Name: "claude_code.Write", Effect: bundlestore.MemberDeny},
			{Kind: bundlestore.MemberTool, ID: 77, Name: "claude_code.Edit", Effect: bundlestore.MemberDeny},
		},
	}}
	problems := checkBundleMembers(resolvingIndex(), bundles)
	if len(problems) != 2 {
		t.Fatalf("want both unresolvable denied tools reported, got %d: %v", len(problems), problems)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{"deny member", "claude_code.Write", "id 77", "claude_code.Edit"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the report must name %q; got:\n%s", want, joined)
		}
	}

	ix := indexFrom(nil, []storeEntry{
		{ID: 13, Name: "playwright"},
		{ID: 76, Name: "claude_code.Write"},
		{ID: 77, Name: "claude_code.Edit"},
	})
	if problems := checkBundleMembers(ix, bundles); len(problems) != 0 {
		t.Fatalf("denied tools that resolve must report nothing, got: %v", problems)
	}
}
