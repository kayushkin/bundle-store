package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	bundlestore "github.com/kayushkin/bundle-store"
)

// This file is the only place on the bundle-store side that turns a curated
// member *name* into the id its owning store assigned. It exists because the
// starter bundles below are written by hand, and a human writes "sql-pro", not
// "1219". Everything downstream of here — the schema, the resolver, the HTTP
// API — carries the id and treats the name as a label.
//
// It is deliberately strict. A name that matches nothing, or that matches more
// than one row, stops the seed with an error that names the candidates. Picking
// one is the failure this whole change removes: skill-store carries the same
// SKILL.md name from more than one upstream source, so "first row wins" means
// a bundle silently installs whichever repository was synced first.

// storeEntry is the shape both skill-store and tool-store list endpoints share.
// Source is skill-store's originating repository; tool-store leaves it empty.
type storeEntry struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source_name"`
}

// memberIndex answers "what id does this name have", and refuses to answer when
// the name has more than one.
type memberIndex struct {
	byKind map[bundlestore.MemberKind]map[string][]storeEntry
}

func (ix *memberIndex) lookup(kind bundlestore.MemberKind, name string) (int64, error) {
	matches := ix.byKind[kind][name]
	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return 0, fmt.Errorf("%s has no %s named %q", kind.OwningStore(), kind, name)
	default:
		return 0, fmt.Errorf(
			"%s has %d %ss named %q (%s) — a bundle member must name the id, not the name",
			kind.OwningStore(), len(matches), kind, name, describeEntries(matches))
	}
}

// confirm answers "does the store still carry this id, under this name". A
// starter member pins an id when its name is ambiguous, and the name written
// beside that id records which row the curator chose: a typo in the id, or a
// row the store has since renamed, would otherwise seed a different member
// without a word. Both are refused, naming what the store holds instead.
func (ix *memberIndex) confirm(kind bundlestore.MemberKind, id int64, name string) error {
	for _, entries := range ix.byKind[kind] {
		for _, e := range entries {
			if e.ID != id {
				continue
			}
			if e.Name != name {
				return fmt.Errorf("%s %s id %d is named %q, not %q — the pinned id no longer names the %s the seed chose",
					kind.OwningStore(), kind, id, e.Name, name, kind)
			}
			return nil
		}
	}
	return fmt.Errorf("%s has no %s with id %d (seeded as %q)", kind.OwningStore(), kind, id, name)
}

func describeEntries(entries []storeEntry) string {
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Source != "" {
			parts = append(parts, fmt.Sprintf("id=%d from %s", e.ID, e.Source))
			continue
		}
		parts = append(parts, fmt.Sprintf("id=%d", e.ID))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

// loadMemberIndex reads every skill and every tool once. Both stores run beside
// this one on the same box; if either cannot be reached the seed fails rather
// than installing a bundle whose members point at nothing.
//
// It reads no environment: the two base URLs are the skill_store_url and
// tool_store_url settings, which main takes from the registry and hands in.
func loadMemberIndex(ctx context.Context, skillStoreBaseURL, toolStoreBaseURL string) (*memberIndex, error) {
	skills, err := fetchEntries(ctx, skillStoreBaseURL+"/skills")
	if err != nil {
		return nil, fmt.Errorf("skill-store: %w", err)
	}
	tools, err := fetchEntries(ctx, toolStoreBaseURL+"/tools")
	if err != nil {
		return nil, fmt.Errorf("tool-store: %w", err)
	}
	return &memberIndex{byKind: map[bundlestore.MemberKind]map[string][]storeEntry{
		bundlestore.MemberSkill: groupByName(skills),
		bundlestore.MemberTool:  groupByName(tools),
	}}, nil
}

func groupByName(entries []storeEntry) map[string][]storeEntry {
	out := map[string][]storeEntry{}
	for _, e := range entries {
		out[e.Name] = append(out[e.Name], e)
	}
	return out
}

func fetchEntries(ctx context.Context, url string) ([]storeEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	var entries []storeEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("GET %s: decode: %w", url, err)
	}
	return entries, nil
}

// memberLookupTimeout bounds the two list calls the seed makes at boot.
const memberLookupTimeout = 15 * time.Second
