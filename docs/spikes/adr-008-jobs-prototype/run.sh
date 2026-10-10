#!/usr/bin/env bash
# ADR-008 prototype driver. This is what gonk-gate would do, written in bash so
# the mechanism is visible rather than buried in a client.
#
# It proves, in order:
#   1. a session's prompt, checkout tarball and virtual key can be delivered as
#      Secret volumes;
#   2. a Job runs the EXISTING L1 synthetic scenario end to end;
#   3. COMPLETION IS READ FROM JOB STATUS -- not from an event stream, and not
#      by polling a session read model that 404s once the bead is closed;
#   4. THE BATCH IS READ FROM POD LOGS, fenced with the same sentinels
#      cmd/gonk-gate/broker_apply.go already uses.
#
# It asserts on FIELDS, never on "the API answered" -- the gitops lesson in
# CLAUDE.md. A missing Job and a failed Job must not look alike.
set -euo pipefail

NS=gonk-spike
JOB=gonk-session-l1
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNDIR="${RUNDIR:-$HERE/../../../.spike-run}"
LOG="$RUNDIR/l1-job-run.log"

mkdir -p "$RUNDIR"
exec > >(tee "$LOG") 2>&1

echo "=== ADR-008 Jobs prototype: L1 synthetic scenario in a Kubernetes Job ==="
echo "date: $(date -Is)"
echo "context: $(kubectl config current-context)"

# REFUSE TO RUN ANYWHERE NEAR PRODUCTION. The spike cluster is disposable; the
# the reference cluster is read-only for this work.
ctx="$(kubectl config current-context)"
case "$ctx" in
	k3d-gonk-t45|kind-gonk-t45) ;;
	*) echo "REFUSING: context '$ctx' is not the disposable spike cluster" >&2; exit 2 ;;
esac

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" delete job "$JOB" --ignore-not-found --wait=true

# --- the three Secret volumes -------------------------------------------------
PROMPT="$RUNDIR/prompt.txt"
cat > "$PROMPT" <<'EOF'
You are the gonk triage agent. Classify the issue and emit your proposed
effects between the GONK_BATCH_START and GONK_BATCH_END sentinels.
EOF

CHECKOUT="$RUNDIR/checkout.tar.gz"
rm -rf "$RUNDIR/checkout-src"; mkdir -p "$RUNDIR/checkout-src"
echo "# synthetic checkout for the ADR-008 spike" > "$RUNDIR/checkout-src/README.md"
tar -czf "$CHECKOUT" -C "$RUNDIR/checkout-src" .

KEY="$RUNDIR/virtual-key"
printf 'sk-spike-virtual-key-not-a-real-credential' > "$KEY"

for s in prompt checkout key; do
	kubectl -n "$NS" delete secret "$JOB-$s" --ignore-not-found >/dev/null
done
kubectl -n "$NS" create secret generic "$JOB-prompt"   --from-file=prompt.txt="$PROMPT"
kubectl -n "$NS" create secret generic "$JOB-checkout" --from-file=checkout.tar.gz="$CHECKOUT"
kubectl -n "$NS" create secret generic "$JOB-key"      --from-file=virtual-key="$KEY"

# --- create the session -------------------------------------------------------
kubectl -n "$NS" apply -f "$HERE/job.yaml"

# --- 3. COMPLETION FROM JOB STATUS -------------------------------------------
echo "--- waiting on Job status (not an event stream) ---"
deadline=$(( $(date +%s) + 600 ))
outcome=""
while [ "$(date +%s)" -lt "$deadline" ]; do
	succeeded=$(kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)
	failed=$(kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status.failed}' 2>/dev/null || true)
	if [ "${succeeded:-0}" -ge 1 ] 2>/dev/null; then outcome=succeeded; break; fi
	if [ "${failed:-0}" -ge 1 ] 2>/dev/null; then outcome=failed; break; fi
	sleep 3
done

if [ -z "$outcome" ]; then
	echo "RESULT: TIMEOUT -- Job never reached a terminal status"
	kubectl -n "$NS" describe job "$JOB" | tail -25
	exit 1
fi
echo "Job terminal status: $outcome"
kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status}' | tr ',' '\n'
echo

# --- 4. BATCH FROM POD LOGS ---------------------------------------------------
pod=$(kubectl -n "$NS" get pods -l "job-name=$JOB" -o jsonpath='{.items[0].metadata.name}')
echo "--- pod: $pod ---"
echo "--- pod labels (ADR-008 section 9 / T-27) ---"
kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.labels}' | tr ',' '\n'
echo
echo "--- full pod log ---"
kubectl -n "$NS" logs "$pod" | tee "$RUNDIR/l1-pod.log"

echo "--- batch extracted from the log ---"
batch=$(sed -n '/GONK_BATCH_START/,/GONK_BATCH_END/p' "$RUNDIR/l1-pod.log" \
	| sed '1d;$d')
if [ -z "$batch" ]; then
	echo "RESULT: FAIL -- no fenced batch in the pod log"
	exit 1
fi
echo "$batch"

# ASSERT ON A FIELD, never on well-formedness.
if ! printf '%s' "$batch" | grep -q '"outcome":"complete"'; then
	echo "RESULT: FAIL -- batch present but outcome is not complete"
	exit 1
fi
if ! printf '%s' "$batch" | grep -q '"l1_exit_code":0'; then
	echo "RESULT: FAIL -- batch outcome claims complete but L1 exit code is non-zero"
	exit 1
fi

[ "$outcome" = "succeeded" ] || { echo "RESULT: FAIL -- batch ok but Job status is $outcome"; exit 1; }

echo "RESULT: PASS -- L1 synthetic scenario ran to completion in a Job;"
echo "               completion read from Job status; batch read from pod logs."
