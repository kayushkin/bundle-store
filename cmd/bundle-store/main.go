// Command bundle-store runs the HTTP registry of curated session bundles and
// the /resolve composition endpoint.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	bundlestore "github.com/kayushkin/bundle-store"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

func main() {
	// checkMembers is the pre-flight the deploy script runs BEFORE it stops the
	// running service. Every starter bundle names its members by name, and the
	// owning store decides whether a name still resolves to exactly one row —
	// so a name that becomes ambiguous upstream turns the next boot into a
	// log.Fatalf. Measured 2026-08-29: skill-store had carried two rows named
	// "browser-automation" since 2026-08-04 (id 297 from antigravity, id 1446
	// from host) and the service had simply not been restarted since, so the
	// failure was 25 days old and invisible. This flag makes it visible without
	// taking the service down to find out.
	checkMembers := flag.Bool("check-members", false,
		"resolve every starter bundle member name against skill-store and tool-store, report, and exit")
	flag.Parse()

	// The registry is built before the check-members branch, not after it: that
	// pre-flight asks skill-store and tool-store, so it needs the two URLs, and
	// a deploy should learn about a misspelled BUNDLE_STORE_ variable from the
	// pre-flight rather than from a service that will not come back up.
	settings, err := bundlestore.NewSettingsRegistry(servicesettings.ProcessEnvironment())
	if err != nil {
		log.Fatalf("read settings: %v", err)
	}
	skillStoreURL := settings.String(bundlestore.SettingSkillStoreURL)
	toolStoreURL := settings.String(bundlestore.SettingToolStoreURL)

	if *checkMembers {
		ctx, cancel := context.WithTimeout(context.Background(), memberLookupTimeout)
		defer cancel()
		index, err := loadMemberIndex(ctx, skillStoreURL, toolStoreURL)
		if err != nil {
			log.Fatalf("check-members: read member ids: %v", err)
		}
		problems := checkStarterMembers(index)
		for _, p := range problems {
			log.Printf("UNRESOLVABLE: %s", p)
		}
		if len(problems) > 0 {
			log.Fatalf("check-members: %d starter bundle member(s) do not resolve — booting would fail the seed", len(problems))
		}
		log.Printf("check-members: every starter bundle member resolves")
		return
	}

	addr := settings.String(bundlestore.SettingListenAddress)

	store, err := bundlestore.Open(settings.String(bundlestore.SettingDataDirectory))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if store.QuarantinedMembers > 0 {
		log.Printf("MIGRATION: %d bundle member(s) were keyed by name and carry no id; "+
			"they have been moved to table %s and are no longer resolved. "+
			"Re-add any hand-curated ones through POST /bundles with the id its owning store assigned.",
			store.QuarantinedMembers, bundlestore.LegacyMembersTable)
	}
	if store.BackfilledExtendsLinks > 0 {
		log.Printf("MIGRATION: %d bundle inheritance link(s) named their parent by name; "+
			"each has been converted to that parent's id and the extends column is gone. "+
			"Renaming a parent bundle no longer strands its children.",
			store.BackfilledExtendsLinks)
	}

	seedCtx, cancelSeed := context.WithTimeout(context.Background(), memberLookupTimeout)
	index, err := loadMemberIndex(seedCtx, skillStoreURL, toolStoreURL)
	cancelSeed()
	if err != nil {
		log.Fatalf("seed bundles: read member ids: %v", err)
	}
	if err := seedBundles(store, index); err != nil {
		log.Fatalf("seed bundles: %v", err)
	}

	mux := http.NewServeMux()
	bundlestore.RegisterHandlers(mux, store)
	bundlestore.RegisterSettingsHandler(mux, settings)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("bundle-store listening on %s (data=%s)", addr, store.DataDir())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// starterBundles is the one authoring of the curated starter bundles. Both the
// boot seed and the pre-flight member check read this list, so a check that
// passes is a statement about the bundles that will actually be seeded.
func starterBundles() []bundlestore.Bundle {
	return []bundlestore.Bundle{
		{
			Name:        "base",
			DisplayName: "Base",
			Description: "Universal base bundle — always applied. Empty by default; a place to hang org-wide defaults later.",
			Enabled:     true,
		},
		{
			Name:        "react",
			DisplayName: "React frontend",
			Description: "Frontend web work: browser-automation skill + Playwright MCP for E2E; Chrome DevTools only when the task is performance.",
			Extends:     "base",
			MatchTags:   []string{"react", "frontend"},
			Members: []bundlestore.Member{
				// Pinned by id, not name. skill-store carries two skills named
				// "browser-automation": id 297 from the antigravity upstream and
				// id 1446, ingested 2026-08-04 from a locally installed
				// ~/.claude/skills. This bundle means the local copy — chosen
				// on 2026-09-10 — and a name lookup rightly refuses
				// to choose between the two.
				{Kind: bundlestore.MemberSkill, ID: 1446, Name: "browser-automation"},
				{Kind: bundlestore.MemberSkill, Name: "senior-frontend"},
				{Kind: bundlestore.MemberTool, Name: "playwright"},
				{Kind: bundlestore.MemberTool, Name: "chrome-devtools", Condition: "perf"},
			},
			Enabled: true,
		},
		{
			Name:        "go-service",
			DisplayName: "Go service",
			Description: "Backend Go service work: backend patterns + SQL skills. No browser tooling.",
			Extends:     "base",
			MatchTags:   []string{"go"},
			Members: []bundlestore.Member{
				{Kind: bundlestore.MemberSkill, Name: "cc-skill-backend-patterns"},
				{Kind: bundlestore.MemberSkill, Name: "sql-pro"},
			},
			Enabled: true,
		},
	}
}

// resolveStarterMember turns one seeded member into the id its owning store
// assigned. A member that pins an ID — because its name alone is ambiguous in
// that store — is confirmed against the store, under the name written beside
// it. A member that gives only a name is looked up, and refused when the name
// is missing or carried more than once.
func resolveStarterMember(index *memberIndex, m bundlestore.Member) (int64, error) {
	if m.ID != 0 {
		if err := index.confirm(m.Kind, m.ID, m.Name); err != nil {
			return 0, err
		}
		return m.ID, nil
	}
	return index.lookup(m.Kind, m.Name)
}

// checkStarterMembers asks the same question the boot seed asks — does every
// member still resolve to exactly one id — and returns one line per member that
// does not, rather than stopping at the first. Reading the whole list matters:
// the seed reports only the first failure, so a deploy that fixes it discovers
// the second one on the next restart.
func checkStarterMembers(index *memberIndex) []string {
	return checkBundleMembers(index, starterBundles())
}

// checkBundleMembers is checkStarterMembers over any list of bundles. It
// checks every member whatever its effect: a denied tool whose id the store no
// longer carries denies nothing, so it fails the check exactly as an included
// one does, and the line says which effect it had.
func checkBundleMembers(index *memberIndex, bundles []bundlestore.Bundle) []string {
	var problems []string
	for _, b := range bundles {
		for _, m := range b.Members {
			if _, err := resolveStarterMember(index, m); err != nil {
				effect := m.Effect
				if effect == "" {
					effect = bundlestore.MemberInclude
				}
				problems = append(problems, fmt.Sprintf("bundle %s: %s member: %v", b.Name, effect, err))
			}
		}
	}
	return problems
}

// seedBundles upserts the curated starter bundles. New rows default to the
// Enabled value in starterBundles; existing rows preserve the operator's
// enabled state, so hand-curation in the UI survives restarts. Definitions
// (members, match tags, inheritance) always refresh from that list — it is the
// canonical source for these starter entries.
//
// The seeds name their members, or pin an id where the name alone is ambiguous;
// index turns each into the id skill-store or tool-store assigned, and a member
// that is missing, ambiguous or pinned to an id under another name stops the
// seed. See members.go for why that is worth a failed boot.
func seedBundles(store *bundlestore.Store, index *memberIndex) error {
	for _, b := range starterBundles() {
		for i := range b.Members {
			id, err := resolveStarterMember(index, b.Members[i])
			if err != nil {
				return fmt.Errorf("bundle %s: %w", b.Name, err)
			}
			b.Members[i].ID = id
		}
		if existing, err := store.GetBundleByName(b.Name); err == nil {
			b.Enabled = existing.Enabled
		} else if !errors.Is(err, bundlestore.ErrNotFound) {
			return fmt.Errorf("lookup bundle %s: %w", b.Name, err)
		}
		if _, err := store.UpsertBundle(&b); err != nil {
			return fmt.Errorf("upsert bundle %s: %w", b.Name, err)
		}
	}
	return nil
}
