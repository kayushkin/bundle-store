#!/usr/bin/env bash
# Boot-and-answer smoke test for bundle-store.
#
# Builds cmd/bundle-store from THIS checkout, boots it against a throwaway SQLite
# file on a throwaway port, and drives real HTTP routes. Proves the committed
# tree produces a binary that BOOTS (a Go 1.22+ ServeMux route conflict panics at
# registration time — it compiles green and dies on start) and ANSWERS.
#
# "ANSWERS" here means POST /resolve, which is the only route with any real logic
# in it: select every enabled bundle that is named "base" or whose match_tags
# intersect the request's repo_tags, expand each through its `extends_id` chain
# ancestor-first, then drop members whose `condition` is not present in task_tags.
# A 200 from /health proves nothing about any of that, so this smoke drives the
# resolver directly — including the conditional gate, which is the part most
# likely to rot silently.
#
# bundle-store SEEDS ITSELF AT BOOT (cmd/bundle-store/main.go upserts base, react
# and go-service on every start), so a fresh temp DB is already meaningful. This
# smoke asserts against those real shipped seeds — read them from main.go, NOT
# from resolve_test.go, whose fixtures deliberately differ.
#
# Never touches live state: the live service runs on :8307 against
# ~/.config/bundle-store/bundle-store.db. BUNDLE_STORE_DATA_DIR is what keeps this
# hermetic — Open("") falls back to that live path and migrates it. HOME is
# redirected too, belt and braces.
#
# resolve.go itself is a pure join over curated definitions and never reaches
# into skill-store or tool-store — it hands back the member ids the curator
# recorded. The BOOT SEED does reach out, once: its members are written by name
# in main.go and it asks each owning store for the id, refusing to start if a
# name is missing or ambiguous. This smoke stubs both stores on throwaway ports
# so it stays hermetic and so it can assert the ids actually travel end to end,
# and it boots a second time against a deliberately ambiguous stub to prove the
# refusal is real.
#
# Exits 0 on success, non-zero on the first failing assertion; dumps the server
# log to stderr on failure.
#
# Tunables:
#   E2E_PORT — bundle-store listen port (default 19115)
#   E2E_STUB_PORT — stub skill-store/tool-store port (default E2E_PORT+1)
#   E2E_KEEP — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PORT="${E2E_PORT:-19115}"
STUB_PORT="${E2E_STUB_PORT:-$((PORT + 1))}"
BASE="http://127.0.0.1:$PORT"
STUB_BASE="http://127.0.0.1:$STUB_PORT"

# The ids the stub registries hand out. The resolve assertions below check for
# these exact numbers, so a member that silently fell back to its name shows up
# as a missing id rather than as a still-green name assertion.
#
# browser-automation is the one member main.go pins by id rather than by name:
# 1446 is the locally installed copy (source "host"), and the stub carries the antigravity row 297 beside
# it, as skill-store does. So this number also proves the pin chose the row.
SKILL_ID_BROWSER_AUTOMATION=1446
SKILL_ID_SENIOR_FRONTEND=1129
SKILL_ID_BACKEND_PATTERNS=320
SKILL_ID_SQL_PRO=1219
TOOL_ID_PLAYWRIGHT=13
TOOL_ID_CHROME_DEVTOOLS=14

# bundle-store uses mattn/go-sqlite3, which is cgo — a C compiler is mandatory or
# `go build` fails with a confusing linker error under the nightly job's minimal env.
for bin in go curl jq cc timeout; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "ERROR: required tool '$bin' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t bundle-store-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
DATA_DIR="$TMP_DIR/data"
BODY="$TMP_DIR/body.json"
mkdir -p "$BIN_DIR" "$DATA_DIR" "$TMP_DIR/fake-home"

NAME="e2e-smoke-$$"

SERVER_PID=""
STUB_PID=""
cleanup() {
  for pid in "$SERVER_PID" "$STUB_PID"; do
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "[e2e] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
fail() {
  echo "FAIL: $*" >&2
  if [ -f "$TMP_DIR/server.log" ]; then
    echo "----- server.log -----" >&2
    cat "$TMP_DIR/server.log" >&2
  fi
  exit 1
}

req() {
  local method="$1" path="$2" data="${3:-}"
  local args=(-sS -o "$BODY" -w '%{http_code}' --max-time 15 -X "$method" "$BASE$path")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' -d "$data")
  fi
  curl "${args[@]}"
}

expect() {
  local want="$1" got="$2" what="$3"
  if [ "$want" != "$got" ]; then
    echo "----- response body -----" >&2
    cat "$BODY" >&2
    echo >&2
    fail "$what: expected HTTP $want, got $got"
  fi
}

jget() { jq -r "$1" "$BODY"; }

assert_eq() {
  [ "$1" = "$2" ] || fail "$3: expected '$1', got '$2'"
}

# The resolve response arrays are unordered sets — never compare them positionally.
# Both take a jq expression yielding an ARRAY, e.g. '[.bundles[].name]' or
# '[.members[].name]', and read the LAST response body.
#
# `index()` is used rather than `jq -e any(...)`: jq -e exits 1 when the filter
# yields false, which is indistinguishable from a broken filter. Compare a value
# in the parent shell instead.
present() {
  local got
  got="$(jq -r "$1 | index(\"$2\") // \"MISSING\"" "$BODY")"
  [ "$got" != "MISSING" ] || {
    echo "----- response body -----" >&2
    cat "$BODY" >&2
    echo >&2
    fail "$3: expected '$2' to be present in $1"
  }
}
absent() {
  local got
  got="$(jq -r "$1 | index(\"$2\") // \"MISSING\"" "$BODY")"
  [ "$got" = "MISSING" ] || {
    echo "----- response body -----" >&2
    cat "$BODY" >&2
    echo >&2
    fail "$3: expected '$2' to be ABSENT from $1"
  }
}

# ============================================================================
step "build cmd/bundle-store from $REPO_DIR"
cd "$REPO_DIR"
CGO_ENABLED=1 go build -o "$BIN_DIR/bundle-store" ./cmd/bundle-store
echo "    binary: $(ls -lh "$BIN_DIR/bundle-store" | awk '{print $5}')"

# ============================================================================
# Stub skill-store and tool-store. bundle-store asks them, once at boot, for the
# id behind each seeded member name, and to confirm the one id the seed pins.
# The normal list already carries two rows called browser-automation, which is
# the live shape: the react bundle pins id 1446, so that name is never looked
# up and the boot goes through. STUB_MODE=ambiguous adds a second row called
# senior-frontend, a member the seed still writes by name, and that one has to
# stop the boot.
step "build the stub registries"
STUB_DIR="$TMP_DIR/stub"
mkdir -p "$STUB_DIR"
cat >"$STUB_DIR/go.mod" <<'GOMOD'
module e2e-stub-registries

go 1.22
GOMOD
cat >"$STUB_DIR/main.go" <<'GOSRC'
// Stub skill-store + tool-store for bundle-store's boot seed. Serves the two
// list routes the seed reads and nothing else.
package main

import (
	"fmt"
	"net/http"
	"os"
)

const skills = `[
  {"id":297,"name":"browser-automation","source_name":"antigravity"},
  {"id":1446,"name":"browser-automation","source_name":"host"},
  {"id":1129,"name":"senior-frontend","source_name":"antigravity"},
  {"id":320,"name":"cc-skill-backend-patterns","source_name":"antigravity"},
  {"id":1219,"name":"sql-pro","source_name":"antigravity"}
]`

// A name the seed does not pin, from two sources — one id each, and no way to
// choose. browser-automation is doubled here too, as in the normal list, so a
// refusal that names senior-frontend cannot be the pinned member's doing.
const ambiguousSkills = `[
  {"id":297,"name":"browser-automation","source_name":"antigravity"},
  {"id":1446,"name":"browser-automation","source_name":"host"},
  {"id":1129,"name":"senior-frontend","source_name":"antigravity"},
  {"id":42,"name":"senior-frontend","source_name":"anthropics"},
  {"id":320,"name":"cc-skill-backend-patterns","source_name":"antigravity"},
  {"id":1219,"name":"sql-pro","source_name":"antigravity"}
]`

const tools = `[
  {"id":13,"name":"playwright"},
  {"id":14,"name":"chrome-devtools"}
]`

func main() {
	body := skills
	if os.Getenv("STUB_MODE") == "ambiguous" {
		body = ambiguousSkills
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/skills", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	})
	mux.HandleFunc("/tools", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, tools)
	})
	if err := http.ListenAndServe(os.Getenv("STUB_ADDR"), mux); err != nil {
		panic(err)
	}
}
GOSRC
(cd "$STUB_DIR" && CGO_ENABLED=0 go build -o "$BIN_DIR/stub-registries" .)

step "boot the stub registries on :$STUB_PORT"
STUB_ADDR=":$STUB_PORT" "$BIN_DIR/stub-registries" >>"$TMP_DIR/stub.log" 2>&1 &
STUB_PID=$!
OK=""
for _ in $(seq 1 75); do
  if ! kill -0 "$STUB_PID" 2>/dev/null; then fail "stub registries exited during startup"; fi
  if curl -fsS --max-time 2 -o /dev/null "$STUB_BASE/skills" 2>/dev/null; then OK=1; break; fi
  sleep 0.2
done
[ -n "$OK" ] || fail "stub registries did not answer on $STUB_BASE"

# ============================================================================
step "boot bundle-store on :$PORT against a throwaway data dir"
BUNDLE_STORE_ADDR=":$PORT" \
BUNDLE_STORE_DATA_DIR="$DATA_DIR" \
SKILL_STORE_URL="$STUB_BASE" \
TOOL_STORE_URL="$STUB_BASE" \
HOME="$TMP_DIR/fake-home" \
  "$BIN_DIR/bundle-store" >>"$TMP_DIR/server.log" 2>&1 &
SERVER_PID=$!
echo "    pid: $SERVER_PID  data dir: $DATA_DIR"

# Poll — never sleep-and-hope. Abort the instant the pid dies: this catches both
# a route-registration panic AND a failed boot-time seed (main.go log.Fatalf's if
# seedBundles fails), which is a real way this binary can die on a good build.
OK=""
for _ in $(seq 1 75); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "bundle-store exited during startup (route-registration panic? boot seed failed?)"
  fi
  if curl -fsS --max-time 2 -o /dev/null "$BASE/health" 2>/dev/null; then OK=1; break; fi
  sleep 0.2
done
[ -n "$OK" ] || fail "bundle-store did not answer /health on $BASE within ~15s"

step "GET /health"
CODE=$(req GET /health); expect 200 "$CODE" "/health"
assert_eq ok "$(jget '.status')" "health status"

# ============================================================================
# The boot seed. A fresh DB is NOT empty — main.go upserts three bundles.
# ============================================================================
step "GET /bundles — the boot seed really ran"
CODE=$(req GET /bundles); expect 200 "$CODE" "GET /bundles"
assert_eq 3 "$(jget 'length')" "seeded bundle count"
for want in base react go-service; do
  assert_eq "$want" "$(jget ".[] | select(.name==\"$want\") | .name")" "seeded bundle '$want' exists"
done

step "GET /bundles/by-name/go-service — the seed's members round-tripped through sqlite"
CODE=$(req GET /bundles/by-name/go-service); expect 200 "$CODE" "GET /bundles/by-name/{name}"
assert_eq base "$(jget '.extends')" "go-service extends base"
present '[.members[].name]' cc-skill-backend-patterns "go-service seed members"
present '[.members[].name]' sql-pro                   "go-service seed members"
present '[.match_tags[]]'     go                        "go-service match_tags"
# The seed wrote names; what got stored is the id the stub registry handed back.
assert_eq "$SKILL_ID_BACKEND_PATTERNS" \
  "$(jget '.members[] | select(.name=="cc-skill-backend-patterns") | .id')" \
  "the boot seed stored skill-store's id, not just the name"
assert_eq "$SKILL_ID_SQL_PRO" "$(jget '.members[] | select(.name=="sql-pro") | .id')" \
  "the boot seed stored skill-store's id, not just the name"

# ============================================================================
# POST /resolve — the money route.
# ============================================================================
step "POST /resolve {repo_tags:[go]} — base + go-service, with their skills"
CODE=$(req POST /resolve '{"repo_tags":["go"]}'); expect 200 "$CODE" "POST /resolve go"
present '[.bundles[].name]' base                      "go resolve selects the universal base bundle"
present '[.bundles[].name]' go-service                "go resolve selects go-service by match_tag"
absent  '[.bundles[].name]' react                     "go resolve must NOT select react"
present '[.skills[].name]'  cc-skill-backend-patterns "go resolve pulls go-service's skills"
present '[.skills[].name]'  sql-pro                   "go resolve pulls go-service's skills"
echo "    bundles: $(jget '[.bundles[].name] | join(", ")')"
echo "    skills:  $(jget '[.skills[].name] | join(", ")')"

step "POST /resolve {repo_tags:[react]} — the OTHER branch, and its conditional tool gate"
CODE=$(req POST /resolve '{"repo_tags":["react"]}'); expect 200 "$CODE" "POST /resolve react"
present '[.bundles[].name]' react                "react resolve selects react"
absent  '[.bundles[].name]' go-service            "react resolve must NOT select go-service"
present '[.skills[].name]'  browser-automation    "react resolve pulls react's skills"
present '[.skills[].name]'  senior-frontend       "react resolve pulls react's skills"
# Per the react seed (cmd/bundle-store/main.go): playwright is UNCONDITIONAL —
# every frontend task gets it — while chrome-devtools carries Condition:"perf",
# i.e. "only when the task is performance". So with no task_tags, playwright is
# in and chrome-devtools is out. That asymmetry IS the feature; assert both
# halves, or a gate that never closes would look identical to a working one.
present '[.tools[].name]'   playwright            "playwright is unconditional for react"
absent  '[.tools[].name]'   chrome-devtools       "condition 'perf' not requested — chrome-devtools must be gated OUT"

step "POST /resolve {repo_tags:[react], task_tags:[perf]} — the gate OPENS"
CODE=$(req POST /resolve '{"repo_tags":["react"],"task_tags":["perf"]}')
expect 200 "$CODE" "POST /resolve react+perf"
present '[.tools[].name]' playwright      "playwright is still there when perf IS requested"
present '[.tools[].name]' chrome-devtools "task_tag 'perf' present — chrome-devtools must be gated IN"
echo "    tools: $(jget '[.tools[].name] | join(", ")')"

step "the resolve response carries the owning store's ids end to end"
# A consumer installs a skill with skill-store's POST /skills/{id}/install —
# there is no install-by-name route — so a resolution that hands back names only
# is not actionable. These are the stub's ids, all the way from the boot seed.
assert_eq "$TOOL_ID_PLAYWRIGHT" "$(jget '.tools[] | select(.name=="playwright") | .id')" \
  "playwright resolves with tool-store's id"
assert_eq "$TOOL_ID_CHROME_DEVTOOLS" "$(jget '.tools[] | select(.name=="chrome-devtools") | .id')" \
  "chrome-devtools resolves with tool-store's id"
assert_eq "$SKILL_ID_BROWSER_AUTOMATION" "$(jget '.skills[] | select(.name=="browser-automation") | .id')" \
  "browser-automation resolves with the id the seed pins, not its upstream twin's"
assert_eq "$SKILL_ID_SENIOR_FRONTEND" "$(jget '.skills[] | select(.name=="senior-frontend") | .id')" \
  "senior-frontend resolves with skill-store's id"
assert_eq 0 "$(jq '[.skills[], .tools[] | select(.id == null or .id <= 0)] | length' "$BODY")" \
  "no resolved member may arrive without an id"
# Contributing bundles are reported by bundle-store's own id too.
assert_eq 0 "$(jq '[.bundles[] | select(.id == null or .id <= 0)] | length' "$BODY")" \
  "no contributing bundle may be reported without its id"

step "POST /resolve {repo_tags:[nothing-matches]} — base still applies, alone"
CODE=$(req POST /resolve '{"repo_tags":["no-such-tag"]}')
expect 200 "$CODE" "POST /resolve unknown tag"
present '[.bundles[].name]' base       "base is universal — it applies to every repo"
assert_eq 1 "$(jget '.bundles | length')" "an unknown repo resolves to base ONLY"
# orderedSet.list() returns [] not null for empties — a null here would break every
# caller that ranges over the result.
assert_eq 0 "$(jget '.skills | length')" "base has no skills"
[ "$(jget '.tools | type')" = "array" ] || fail ".tools must be [] not null when empty: $(cat "$BODY")"

# ============================================================================
# Write path: a bundle of our own, then resolve through it.
# ============================================================================
step "POST /bundles — create a bundle with a conditional member"
# `enabled` defaults to Go's zero value (false) when omitted, and Resolve only
# ever considers enabled bundles — so a bundle POSTed without "enabled":true
# round-trips on GET but is INVISIBLE to /resolve. Send it explicitly.
CODE=$(req POST /bundles \
  "{\"name\":\"$NAME\",\"display_name\":\"E2E Smoke\",\"match_tags\":[\"e2e-smoketag\"],\"enabled\":true,\"members\":[{\"kind\":\"tool\",\"id\":9001,\"name\":\"e2e-always\"},{\"kind\":\"tool\",\"id\":9002,\"name\":\"e2e-gated\",\"condition\":\"e2e-cond\"},{\"kind\":\"skill\",\"id\":9003,\"name\":\"e2e-skill\"}]}")
expect 201 "$CODE" "POST /bundles"
BUNDLE_ID="$(jget '.id')"
[ -n "$BUNDLE_ID" ] && [ "$BUNDLE_ID" != "null" ] || fail "POST /bundles returned no id"
echo "    bundle id: $BUNDLE_ID"

step "GET /bundles/by-name/{name} — members round-tripped, condition preserved"
CODE=$(req GET "/bundles/by-name/$NAME"); expect 200 "$CODE" "GET /bundles/by-name"
assert_eq 3 "$(jget '.members | length')" "member count"
assert_eq e2e-cond "$(jget '.members[] | select(.name=="e2e-gated") | .condition')" \
  "the member's condition survived the round trip"

step "POST /resolve — our bundle's gate behaves like the seeded one"
CODE=$(req POST /resolve '{"repo_tags":["e2e-smoketag"]}')
expect 200 "$CODE" "POST /resolve e2e-smoketag"
present '[.bundles[].name]' "$NAME"     "our bundle was selected by its match_tag"
present '[.tools[].name]'   e2e-always  "the unconditional member resolves"
present '[.skills[].name]'  e2e-skill   "the skill member resolves"
absent  '[.tools[].name]'   e2e-gated   "the gated member is filtered out with no task_tags"

CODE=$(req POST /resolve '{"repo_tags":["e2e-smoketag"],"task_tags":["e2e-cond"]}')
expect 200 "$CODE" "POST /resolve e2e-smoketag + condition"
present '[.tools[].name]' e2e-gated "the gated member resolves once its condition is requested"
assert_eq 0 "$(jget '.denied_tools | length')" "nothing denied yet"
assert_eq array "$(jget '.denied_read_paths | type')" ".denied_read_paths must be [] not null when empty"

step "GET /member-effects — the one list an effect is checked against"
CODE=$(req GET /member-effects); expect 200 "$CODE" "GET /member-effects"
assert_eq '["include","deny"]' "$(jq -c . "$BODY")" "member effects"

step "POST /bundles — a second bundle denies a tool the first includes, and a path"
DENY_NAME="$NAME-deny"
CODE=$(req POST /bundles \
  "{\"name\":\"$DENY_NAME\",\"match_tags\":[\"e2e-smoketag\"],\"enabled\":true,\"denied_read_paths\":[\"~/.config/**\",\"/proc/**\"],\"members\":[{\"kind\":\"tool\",\"id\":9001,\"name\":\"e2e-always\",\"effect\":\"deny\"}]}")
expect 201 "$CODE" "POST /bundles with a deny"
CODE=$(req POST /bundles "{\"name\":\"$DENY_NAME-bad\",\"members\":[{\"kind\":\"skill\",\"id\":9003,\"effect\":\"deny\"}]}")
expect 400 "$CODE" "a denied skill is refused"
CODE=$(req POST /bundles "{\"name\":\"$DENY_NAME-bad\",\"denied_read_paths\":[\"relative/**\"]}")
expect 400 "$CODE" "a relative denied read path is refused"

step "POST /resolve — a deny anywhere wins"
CODE=$(req POST /resolve '{"repo_tags":["e2e-smoketag"]}')
expect 200 "$CODE" "POST /resolve e2e-smoketag with a deny"
absent  '[.tools[].name]'        e2e-always "a tool one bundle denies is gone even though another includes it"
present '[.denied_tools[].name]' e2e-always "and it is listed as denied"
present '.denied_read_paths'   '~/.config/**' "the denied read path comes through as written"
present '.denied_read_paths'   '/proc/**'     "the denied read path comes through as written"
CODE=$(req DELETE "/bundles/$(curl -s "$BASE/bundles/by-name/$DENY_NAME" | jq -r .id)")
expect 200 "$CODE" "DELETE the deny bundle"

step "POST /bundles/{id}/disable — a disabled bundle disappears from /resolve"
CODE=$(req POST "/bundles/$BUNDLE_ID/disable" '')
[ "$CODE" = 200 ] || [ "$CODE" = 204 ] || fail "POST disable: expected 200/204, got $CODE"
CODE=$(req POST /resolve '{"repo_tags":["e2e-smoketag"]}')
expect 200 "$CODE" "POST /resolve after disable"
absent '[.bundles[].name]' "$NAME"    "a disabled bundle must not resolve"
absent '[.tools[].name]'   e2e-always "a disabled bundle contributes no members"
present '[.bundles[].name]' base      "…but base still does"

step "POST /bundles/{id}/enable — and it comes back"
CODE=$(req POST "/bundles/$BUNDLE_ID/enable" '')
[ "$CODE" = 200 ] || [ "$CODE" = 204 ] || fail "POST enable: expected 200/204, got $CODE"
CODE=$(req POST /resolve '{"repo_tags":["e2e-smoketag"]}')
expect 200 "$CODE" "POST /resolve after re-enable"
present '[.bundles[].name]' "$NAME" "the re-enabled bundle resolves again"

step "POST /bundles — an invalid member kind is rejected"
CODE=$(req POST /bundles \
  '{"name":"e2e-bad-kind","enabled":true,"members":[{"kind":"frobnicate","id":1,"name":"nope"}]}')
expect 400 "$CODE" "POST /bundles with kind=frobnicate"

step "POST /bundles — a member with a name and no id is rejected"
# This is the write that used to succeed and produce an uninstallable member.
CODE=$(req POST /bundles \
  '{"name":"e2e-name-only","enabled":true,"members":[{"kind":"skill","name":"claude-api"}]}')
expect 400 "$CODE" "POST /bundles with a name-only member"
grep -q 'skill-store' "$BODY" || fail "the rejection must name the store that owns the id: $(cat "$BODY")"
CODE=$(req GET /bundles/by-name/e2e-name-only)
expect 404 "$CODE" "a rejected bundle must not be half-written"

step "POST /bundles — a bundle extending a missing parent is refused at the write"
# This write used to be accepted, because `extends` held a name and nothing
# checked it. The failure then surfaced on somebody else's /resolve. The link is
# the parent's id now, so there is nothing to write and the refusal lands here.
CODE=$(req POST /bundles \
  '{"name":"e2e-orphan","extends":"no-such-parent","match_tags":["e2e-orphan-tag"],"enabled":true}')
expect 400 "$CODE" "POST /bundles with a dangling extends"
grep -q 'no-such-parent' "$BODY" || fail "the rejection must name the parent it could not find: $(cat "$BODY")"
CODE=$(req GET /bundles/by-name/e2e-orphan)
expect 404 "$CODE" "a rejected bundle must not be half-written"

step "POST /bundles — the inheritance link that IS stored is the parent's id"
CODE=$(req GET /bundles/by-name/react); expect 200 "$CODE" "GET /bundles/by-name/react"
REACT_EXTENDS_ID=$(jget '.extends_id')
CODE=$(req GET /bundles/by-name/base); expect 200 "$CODE" "GET /bundles/by-name/base"
assert_eq "$(jget '.id')" "$REACT_EXTENDS_ID" "react's extends_id is base's real id, not a name"

step "DELETE /bundles/{id} — a bundle other bundles extend is refused, and names them"
# ON DELETE RESTRICT. Under SET NULL react would quietly become a root bundle
# and stop inheriting anything, with no error anywhere.
CODE=$(req DELETE "/bundles/$REACT_EXTENDS_ID")
expect 409 "$CODE" "DELETE a bundle that other bundles extend"
grep -q 'react' "$BODY" || fail "the refusal must name the children: $(cat "$BODY")"
CODE=$(req GET /bundles/by-name/base); expect 200 "$CODE" "the refused delete must not have removed the parent"

step "GET /bundles/{id} — unknown id is a 404, not a panic"
CODE=$(req GET /bundles/999999); expect 404 "$CODE" "GET unknown bundle"

# ============================================================================
step "DELETE /bundles/{id} — and it is really gone"
CODE=$(req DELETE "/bundles/$BUNDLE_ID")
[ "$CODE" = 200 ] || [ "$CODE" = 204 ] || fail "DELETE /bundles/{id}: expected 200/204, got $CODE"
CODE=$(req GET "/bundles/by-name/$NAME"); expect 404 "$CODE" "GET deleted bundle"
CODE=$(req POST /resolve '{"repo_tags":["e2e-smoketag"]}')
expect 200 "$CODE" "POST /resolve after delete"
absent '[.bundles[].name]' "$NAME" "the deleted bundle no longer resolves"

step "the server is still alive after all of that"
CODE=$(req GET /health); expect 200 "$CODE" "/health at end of run"

# ============================================================================
# The refusal, driven for real. A registry carrying one name twice is the live
# shape, and for a member the seed writes by name the only safe answer is to
# stop — picking a row would install whichever upstream repository happened to
# sync first. The first boot above already passed the other half: the same
# doubling on browser-automation, which the seed pins by id, did not stop it.
# ============================================================================
step "a seeded member name the registry carries twice stops the boot"
kill "$STUB_PID" 2>/dev/null || true
wait "$STUB_PID" 2>/dev/null || true
STUB_ADDR=":$STUB_PORT" STUB_MODE=ambiguous "$BIN_DIR/stub-registries" >>"$TMP_DIR/stub.log" 2>&1 &
STUB_PID=$!
OK=""
for _ in $(seq 1 75); do
  if curl -fsS --max-time 2 -o /dev/null "$STUB_BASE/skills" 2>/dev/null; then OK=1; break; fi
  sleep 0.2
done
[ -n "$OK" ] || fail "ambiguous stub registry did not come up"

# A boot that ought to be refused exits within a second. One that is not
# refused serves forever, so without a bound this script hangs instead of
# failing; timeout(1) ends it and reports 124.
REFUSED_BOOT_LIMIT_SECONDS=20

AMBIG_DIR="$TMP_DIR/ambiguous-data"
mkdir -p "$AMBIG_DIR"
set +e
BUNDLE_STORE_ADDR=":$((PORT + 2))" \
BUNDLE_STORE_DATA_DIR="$AMBIG_DIR" \
SKILL_STORE_URL="$STUB_BASE" \
TOOL_STORE_URL="$STUB_BASE" \
HOME="$TMP_DIR/fake-home" \
  timeout "$REFUSED_BOOT_LIMIT_SECONDS" "$BIN_DIR/bundle-store" >"$TMP_DIR/ambiguous.log" 2>&1
AMBIG_EXIT=$?
set -e
[ "$AMBIG_EXIT" -ne 124 ] || fail "bundle-store was still serving after ${REFUSED_BOOT_LIMIT_SECONDS}s despite an ambiguous member name — it picked a row"
[ "$AMBIG_EXIT" -ne 0 ] || fail "bundle-store started despite an ambiguous member name — it picked a row"
grep -q 'named "senior-frontend"' "$TMP_DIR/ambiguous.log" \
  || fail "the refusal must name the ambiguous member: $(cat "$TMP_DIR/ambiguous.log")"
grep -q 'anthropics' "$TMP_DIR/ambiguous.log" \
  || fail "the refusal must name the candidate sources: $(cat "$TMP_DIR/ambiguous.log")"
echo "    refused: $(tail -n 1 "$TMP_DIR/ambiguous.log")"

step "an unreachable registry stops the boot too"
kill "$STUB_PID" 2>/dev/null || true
wait "$STUB_PID" 2>/dev/null || true
STUB_PID=""
DOWN_DIR="$TMP_DIR/down-data"
mkdir -p "$DOWN_DIR"
set +e
BUNDLE_STORE_ADDR=":$((PORT + 3))" \
BUNDLE_STORE_DATA_DIR="$DOWN_DIR" \
SKILL_STORE_URL="$STUB_BASE" \
TOOL_STORE_URL="$STUB_BASE" \
HOME="$TMP_DIR/fake-home" \
  timeout "$REFUSED_BOOT_LIMIT_SECONDS" "$BIN_DIR/bundle-store" >"$TMP_DIR/down.log" 2>&1
DOWN_EXIT=$?
set -e
[ "$DOWN_EXIT" -ne 124 ] || fail "bundle-store was still serving after ${REFUSED_BOOT_LIMIT_SECONDS}s with no registry to read ids from"
[ "$DOWN_EXIT" -ne 0 ] || fail "bundle-store started with no registry to read ids from"
echo "    refused: $(tail -n 1 "$TMP_DIR/down.log")"

# ============================================================================
step "hermeticity: the live db was never opened"
if [ -e "$TMP_DIR/fake-home/.config/bundle-store" ]; then
  fail "bundle-store wrote to \$HOME/.config/bundle-store despite BUNDLE_STORE_DATA_DIR — the env var is being ignored"
fi
[ -f "$DATA_DIR/bundle-store.db" ] || fail "the throwaway db was never created at $DATA_DIR/bundle-store.db"
echo "    only $DATA_DIR/bundle-store.db was written"

step "SUCCESS — bundle-store boots and answers from this tree"
echo "    server log: $TMP_DIR/server.log"
