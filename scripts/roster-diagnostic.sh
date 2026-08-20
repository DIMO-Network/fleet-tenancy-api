#!/usr/bin/env bash
#
# roster-diagnostic.sh — the one-time read plan 07 step 3 asks a human to do.
#
# Compares kaufmann_oracle.vins, fleets_lite.vehicles and (once populated) this
# service's roster, and reports where they disagree about who owns what.
#
# WHY THIS IS A SCRIPT AND NOT PART OF THE SERVICE
#
# Each service's database user is scoped to its own schema: fleet_tenancy_api's
# credentials cannot read fleets_lite or kaufmann_oracle. That is a deliberate
# isolation property and not worth weakening for a diagnostic, so the one place
# this comparison can run is a workstation holding all three sets of
# credentials. Nothing in the service depends on it, and it is expected to be
# run once, read, and then only re-run if somebody wants the number again.
#
# WHAT IT WILL SHOW, from the 2026-08-19 investigation:
#
#   owner DIFFER            3    tokens 192379, 192400, 192401 — Maxus T60s.
#                                Kaufmann says 0xDA13fE28…; fleet-lite and
#                                identity-api both say 0x97B8bA44…. Kaufmann is
#                                wrong, and has been since a transfer.
#   kaufmann-only          27    onboarded, not in any synced fleet
#   fleet-lite-only       178    never came through kaufmann — self-serve
#                                tenants' own licences
#
# The first two are stable; the third is not. Re-run 2026-08-19 22:0x UTC gave
# 3 / 27 / 179 — one more self-serve vehicle than the morning's count, which is
# what a live platform does between two measurements. Treat the contradiction
# count as the assertion and the population counts as context: 3 becoming 4 is
# a finding, 179 becoming 180 is a Tuesday.
#
# READ IT, DO NOT ACT ON IT. The reconcile job already corrects owner from the
# chain; this exists so a human sees the divergence once, with its shape, rather
# than only ever seeing the corrected state. Nothing here writes.
#
# USAGE
#
#   ssh dimo-database-prod                      # LocalForward 5430 -> RDS:5432
#   # Take each service's own credentials from its k8s secret — the users are
#   # schema-scoped, so one set will not read the others:
#   #   kubectl -n prod get secret <svc>-secret -o jsonpath='{.data.DB_USER}' | base64 -d
#   export PGHOST=localhost PGPORT=5430 PGSSLMODE=require
#   export KAUF_USER=… KAUF_PASS=… FL_USER=… FL_PASS=… FTA_USER=… FTA_PASS=…
#   ./scripts/roster-diagnostic.sh
#
set -euo pipefail

: "${PGHOST:=localhost}"
: "${PGPORT:=5430}"
: "${PGSSLMODE:=require}"
export PGHOST PGPORT PGSSLMODE

for v in KAUF_USER KAUF_PASS FL_USER FL_PASS; do
  if [ -z "${!v:-}" ]; then
    echo "missing required env var: $v" >&2
    exit 2
  fi
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "== pulling kaufmann_oracle.vins =="
PGPASSWORD="$KAUF_PASS" psql -U "$KAUF_USER" -d kaufmann_oracle -tAF'|' -c \
  "SELECT vehicle_token_id, lower(owner)
     FROM kaufmann_oracle.vins
    WHERE vehicle_token_id IS NOT NULL AND owner IS NOT NULL AND owner <> ''
    ORDER BY vehicle_token_id;" > "$work/kauf.psv"

echo "== pulling fleets_lite.vehicles =="
PGPASSWORD="$FL_PASS" psql -U "$FL_USER" -d fleets_lite -tAF'|' -c \
  "SELECT DISTINCT token_id, lower(owner_address)
     FROM fleets_lite.vehicles
    WHERE owner_address IS NOT NULL AND owner_address <> ''
    ORDER BY token_id;" > "$work/fl.psv"

echo
echo "== counts =="
printf 'kaufmann rows with an owner : %s\n' "$(wc -l < "$work/kauf.psv" | tr -d ' ')"
printf 'fleet-lite rows with an owner: %s\n' "$(wc -l < "$work/fl.psv" | tr -d ' ')"

echo
echo "== owner CONTRADICTIONS (both hold the token, owners differ) =="
echo "   these are the rows that justify the roster; expect 3"
join -t'|' -1 1 -2 1 \
  <(sort -t'|' -k1,1 "$work/kauf.psv") \
  <(sort -t'|' -k1,1 "$work/fl.psv") \
  | awk -F'|' '$2 != $3 { printf "token %-10s kaufmann=%s  fleet-lite=%s\n", $1, $2, $3 }' \
  | sort -n -k2 | tee "$work/differ.txt"
printf 'contradictions: %s\n' "$(wc -l < "$work/differ.txt" | tr -d ' ')"

echo
echo "== kaufmann-only tokens (onboarded, in no synced fleet); expect 27 =="
comm -23 <(cut -d'|' -f1 "$work/kauf.psv" | sort -u) <(cut -d'|' -f1 "$work/fl.psv" | sort -u) \
  | tee "$work/kauf_only.txt" | tr '\n' ' '
echo
printf 'kaufmann-only: %s\n' "$(wc -l < "$work/kauf_only.txt" | tr -d ' ')"

echo
echo "== fleet-lite-only tokens (never came through kaufmann); expect 178 =="
comm -13 <(cut -d'|' -f1 "$work/kauf.psv" | sort -u) <(cut -d'|' -f1 "$work/fl.psv" | sort -u) \
  > "$work/fl_only.txt"
printf 'fleet-lite-only: %s\n' "$(wc -l < "$work/fl_only.txt" | tr -d ' ')"
echo "   (not listed; it is the self-serve population and the reason the roster"
echo "    cannot live in kaufmann — it would have a permanent hole this size)"

roster_exists() {
  local n
  n=$(PGPASSWORD="$FTA_PASS" psql -U "$FTA_USER" -d fleet_tenancy_api -tAc \
    "SELECT COUNT(*) FROM information_schema.tables
      WHERE table_schema='fleet_tenancy_api' AND table_name='vehicles';" 2>/dev/null || echo 0)
  [ "${n:-0}" = "1" ]
}

if [ -n "${FTA_USER:-}" ] && [ -n "${FTA_PASS:-}" ] && roster_exists; then
  echo
  echo "== roster coverage =="
  PGPASSWORD="$FTA_PASS" psql -U "$FTA_USER" -d fleet_tenancy_api -tAF'|' -c \
    "SELECT vehicle_token_id, lower(owner)
       FROM fleet_tenancy_api.vehicles
      WHERE owner IS NOT NULL
      ORDER BY vehicle_token_id;" > "$work/roster.psv"
  printf 'roster rows with an owner: %s\n' "$(wc -l < "$work/roster.psv" | tr -d ' ')"

  echo
  echo "-- tokens the roster is missing that one of the two source tables has --"
  comm -13 <(cut -d'|' -f1 "$work/roster.psv" | sort -u) \
           <(cat <(cut -d'|' -f1 "$work/kauf.psv") <(cut -d'|' -f1 "$work/fl.psv") | sort -u) \
    | tee "$work/missing.txt" | head -40
  printf 'missing from roster: %s\n' "$(wc -l < "$work/missing.txt" | tr -d ' ')"
  echo "   A non-zero count is worth understanding rather than assuming: the"
  echo "   roster's population is the union of privileged sets over the licences"
  echo "   THIS service holds, so a vehicle here but not there means a licence"
  echo "   it cannot see — a self-serve tenant whose credentials never made it"
  echo "   into tenant_credentials, or an SACD that has since been revoked."

  echo
  echo "-- where the roster and kaufmann disagree on owner --"
  echo "   the roster reads the chain, so these are kaufmann's errors"
  join -t'|' -1 1 -2 1 \
    <(sort -t'|' -k1,1 "$work/roster.psv") \
    <(sort -t'|' -k1,1 "$work/kauf.psv") \
    | awk -F'|' '$2 != $3 { printf "token %-10s roster/chain=%s  kaufmann=%s\n", $1, $2, $3 }'
else
  echo
  echo "(skipping roster coverage: either FTA_USER/FTA_PASS are unset, or the"
  echo " vehicles table does not exist yet in the environment you are pointed"
  echo " at — which is the expected state before plan 07 step 3 is deployed.)"
fi

echo
echo "done. nothing was written."
