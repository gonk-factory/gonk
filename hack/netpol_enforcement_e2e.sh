#!/usr/bin/env bash
# Prove the gonk-agent egress policy is CORRECT, on a cluster that enforces it.
#
# WHY THIS NEEDS ITS OWN CLUSTERS (gonk-v47). On a cluster whose CNI has no
# policy controller, every NetworkPolicy in this chart is accepted by the API
# server and enforced by nothing (gonk-dku, measurable with `helm test gonk`).
# An unprivileged CI runner cannot start kind, k3s or DinD either. So this runs
# where real Docker is available, for example a GitHub runner.
#
# WHAT IT TESTS, precisely: the POLICY, not intake's business logic. The
# destinations are stand-ins carrying the exact labels and ports the policy
# selects on, because the policy cannot tell the difference and the real intake
# would need GitLab, a meter and five Secrets to start. The PROBE is the real
# one, built from this repo.
#
# THE NEGATIVE CONTROL IS THE POINT. A probe that always said ENFORCED would
# pass the positive case silently. So this runs twice: with the policies applied
# it must report ENFORCED, and with them deleted it must report NOT ENFORCED.
# Only both together mean anything.
#
# AND THREE CONNECTIONS THE POLICIES MUST PERMIT (gonk-nax7, gonk-vxel). The
# probe above shows that the policies BLOCK. It cannot show that they still let
# gonk work, and in v0.2.0 they did not: on a cluster that enforces them the
# agent could not fetch its checkout, the controller could not register one,
# and neither the controller nor the meter could reach the Kubernetes API
# server, so no session Job was ever created. Each of those is observed here
# three times, from pods carrying the real labels:
#   1. with no policy at all        -> reachable (the stand-ins and paths work)
#   2. with the v0.2.2 policy shape -> BLOCKED   (the defect, reproduced)
#   3. with the chart's policies    -> reachable (the fix)
# Step 2 is the negative control: without it, step 3 passing would not show
# that the new rules are what made the difference.
set -euo pipefail

NS=${NS:-gonk-netpol-e2e}
IMAGE=${IMAGE:?set IMAGE to the gonk-intake image loaded into the cluster}
# agnhost is the upstream Kubernetes network-test image; netexec serves HTTP on
# a port and does nothing else. Pinned: a floating tag here would silently
# change what "the destination" is.
AGNHOST=${AGNHOST:-registry.k8s.io/e2e-test-images/agnhost:2.53}

HELM=${HELM:-helm}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

log() { printf '\n=== %s\n' "$*"; }

# WHICH CLUSTER. This script creates a namespace, pods and NetworkPolicies and
# deletes them again, so it must never run against a cluster somebody uses. It
# takes the context from KUBE_CONTEXT, else kubectl's current one, refuses any
# name that is not a throwaway k3d or kind cluster, and passes that context on
# EVERY call: the current context is shared state on a development machine and
# can change under a running script.
KUBE_CONTEXT=${KUBE_CONTEXT:-$(command kubectl config current-context 2>/dev/null || true)}
case "$KUBE_CONTEXT" in
  k3d-*|kind-*) ;;
  *)
    echo "refusing to run: kube context '${KUBE_CONTEXT}' is not a k3d-* or kind-* cluster." >&2
    echo "Set KUBE_CONTEXT to a throwaway cluster's context, for example KUBE_CONTEXT=k3d-gonk." >&2
    exit 2
    ;;
esac
kubectl() { command kubectl --context "$KUBE_CONTEXT" "$@"; }

log "namespace $NS"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
# The token controller creates the namespace's `default` ServiceAccount
# asynchronously. A pod applied before it exists is refused with
# `serviceaccount "default" not found` (seen on 2026-09-08, run 34291536045),
# so wait for it rather than race it.
for _ in $(seq 1 30); do
  kubectl -n "$NS" get serviceaccount default >/dev/null 2>&1 && break
  sleep 1
done
kubectl -n "$NS" get serviceaccount default >/dev/null

log "destination stand-ins (the labels and ports the policy selects on)"
kubectl -n "$NS" apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: intake
  labels:
    app.kubernetes.io/name: gonk
    app.kubernetes.io/instance: gonk
    app.kubernetes.io/component: intake
spec:
  containers:
    # ONE POD, TWO PORTS, as in the real intake: 8080 is the public (webhook)
    # listener, which no pod in this namespace may reach, and 9090 is the
    # private one, where the controller registers a checkout and the agent
    # fetches it.
    - name: public
      image: $AGNHOST
      args: ["netexec", "--http-port=8080"]
    - name: private
      image: $AGNHOST
      args: ["netexec", "--http-port=9090", "--udp-port=-1"]
---
# A pod with the AGENT's labels that stays up, so a connection can be tried
# from it before, between and after the policy changes. (The probe pods below
# carry the same labels and run to completion.)
apiVersion: v1
kind: Pod
metadata:
  name: agent
  labels:
    app: gc-agent
spec:
  containers:
    - name: client
      image: $AGNHOST
      args: ["pause"]
---
apiVersion: v1
kind: Pod
metadata:
  name: controller
  labels:
    app.kubernetes.io/name: gonk
    app.kubernetes.io/instance: gonk
    app.kubernetes.io/component: controller
spec:
  containers:
    - name: api
      image: $AGNHOST
      args: ["netexec", "--http-port=9443"]
---
apiVersion: v1
kind: Pod
metadata:
  name: meter
  labels:
    app.kubernetes.io/name: gonk
    app.kubernetes.io/instance: gonk
    app.kubernetes.io/component: meter
spec:
  containers:
    - name: http
      image: $AGNHOST
      args: ["netexec", "--http-port=8080"]
---
apiVersion: v1
kind: Service
metadata: {name: gonk-meter}
spec:
  selector: {app.kubernetes.io/component: meter}
  ports: [{port: 8080, targetPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: gonk-intake}
spec:
  selector: {app.kubernetes.io/component: intake}
  ports: [{port: 8080, targetPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: gonk-intake-internal}
spec:
  selector: {app.kubernetes.io/component: intake}
  ports: [{port: 9090, targetPort: 9090}]
---
apiVersion: v1
kind: Service
metadata: {name: gonk-controller}
spec:
  selector: {app.kubernetes.io/component: controller}
  ports: [{port: 9443, targetPort: 9443}]
EOF
kubectl -n "$NS" wait --for=condition=Ready pod/intake pod/meter pod/controller pod/agent --timeout=180s

# WHERE THE API SERVER IS, read the way chart/gonk/README.md (section 0) tells
# an operator to read it: the addresses and port BEHIND the `kubernetes`
# Service. That is what goes into networkPolicy.kubeAPI. What a pod DIALS is the
# Service's cluster IP on 443 (client-go's in-cluster config), so that is what
# the checks below dial.
API_ADDRS=$(kubectl -n default get endpointslices -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{range .items[*]}{range .endpoints[*]}{.addresses[0]}{"\n"}{end}{end}' | sort -u)
API_PORT=$(kubectl -n default get endpointslices -l kubernetes.io/service-name=kubernetes \
  -o jsonpath='{.items[0].ports[0].port}')
API_SVC=$(kubectl -n default get service kubernetes -o jsonpath='{.spec.clusterIP}')
[ -n "$API_ADDRS" ] && [ -n "$API_PORT" ] && [ -n "$API_SVC" ] || {
  echo "FAIL: could not read the kubernetes Service and its endpoints"; exit 1; }
API_CIDRS=""
for a in $API_ADDRS; do
  case "$a" in *:*) API_CIDRS="$API_CIDRS${API_CIDRS:+,}$a/128";; *) API_CIDRS="$API_CIDRS${API_CIDRS:+,}$a/32";; esac
done
INTAKE_PRIVATE=$(kubectl -n "$NS" get service gonk-intake-internal -o jsonpath='{.spec.clusterIP}'):9090
echo "API server endpoints: $API_CIDRS port $API_PORT (dialled as $API_SVC:443)"
echo "intake private listener: $INTAKE_PRIVATE"

# The policies straight out of the chart -- not hand-written here, or this would
# test a copy rather than what ships.
log "rendering the chart's NetworkPolicies"
# ALL of them, not just the agent's. NetworkPolicy is enforced on EGRESS AT THE
# SOURCE and on INGRESS AT THE DESTINATION, so applying only the agent policy
# would leave every destination's default-deny ingress in force and the allow leg
# would fail for a reason that has nothing to do with what is being tested.
#
# TWO RENDERS. $WORK/netpol.yaml is what ships, with networkPolicy.kubeAPI set
# for THIS cluster. $WORK/netpol-v022.yaml is the same render put back into the
# shape v0.2.2 shipped: no API-server rule, and gonk-intake admitting no pod of
# this namespace. It is the negative control for the three connections above.
NS="$NS" WORK="$WORK" HELM="$HELM" API_CIDRS="$API_CIDRS" API_PORT="$API_PORT" python3 - <<'PYEOF'
import os, subprocess, yaml

def render(*extra):
    out = subprocess.run(
        [os.environ.get("HELM", "helm"), "template", "gonk", "chart/gonk",
         "--namespace", os.environ["NS"],
         "--values", "chart/gonk/ci/values-default.yaml", *extra],
        capture_output=True, text=True, check=True).stdout
    return [d for d in yaml.safe_load_all(out) if d and d.get("kind") == "NetworkPolicy"]

def write(name, docs):
    with open(os.path.join(os.environ["WORK"], name), "w") as f:
        f.write(yaml.safe_dump_all(docs))

ships = render("--set", "networkPolicy.kubeAPI.cidrs={%s}" % os.environ["API_CIDRS"],
               "--set", "networkPolicy.kubeAPI.ports={%s}" % os.environ["API_PORT"])
write("netpol.yaml", ships)

# The v0.2.2 shape. Asserted, not assumed: if the render stops containing the
# rules this strips, the negative control would be testing nothing.
old = render()   # networkPolicy.kubeAPI unset: no API-server rule
stripped = 0
for d in old:
    if any("ipBlock" in peer for rule in d["spec"].get("egress", []) for peer in rule.get("to", [])):
        raise SystemExit("the default render carries an ipBlock rule; the negative control is void")
    if d["metadata"]["name"] != "gonk-intake":
        continue
    kept = []
    for rule in d["spec"]["ingress"]:
        same_ns = [p for p in rule.get("from", []) if "podSelector" in p and "namespaceSelector" not in p]
        if same_ns:
            stripped += 1
            continue
        kept.append(rule)
    d["spec"]["ingress"] = kept
if stripped != 1:
    raise SystemExit("expected to strip exactly 1 same-namespace ingress rule from gonk-intake, stripped %d" % stripped)
with_api = sum(1 for d in ships
               if any("ipBlock" in peer for rule in d["spec"].get("egress", []) for peer in rule.get("to", [])))
if with_api != 2:
    raise SystemExit("expected the API-server rule on exactly 2 policies (controller, meter), found %d" % with_api)
write("netpol-v022.yaml", old)
PYEOF
grep -c 'kind: NetworkPolicy' "$WORK/netpol.yaml"

# connect FROM-POD TARGET: one TCP connection attempt from inside a pod. Prints
# nothing and returns 0 when it connects; prints why (REFUSED, TIMEOUT, ...)
# and returns non-zero when it does not. kube-router REJECTS a denied
# connection and Calico DROPS it, so both REFUSED and TIMEOUT mean "blocked".
connect() {
  kubectl -n "$NS" exec "$1" -c "${3:-}" -- /agnhost connect --timeout=3s "$2" 2>&1 | head -n 1 || true
}
container_of() { case "$1" in intake) echo public;; agent) echo client;; controller) echo api;; meter) echo http;; esac; }

# must_reach WHAT FROM-POD TARGET: a policy change is not programmed instantly,
# so try for up to 60s and require the connection to succeed.
must_reach() {
  local what=$1 from=$2 target=$3 why=""
  for _ in $(seq 1 30); do
    why=$(connect "$from" "$target" "$(container_of "$from")")
    if [ -z "$why" ]; then printf '  ok  reached  %-34s %s -> %s\n' "$what" "$from" "$target"; return 0; fi
    sleep 2
  done
  printf '  !!  BLOCKED  %-34s %s -> %s (%s)\n' "$what" "$from" "$target" "$why"
  return 1
}

# must_block WHAT FROM-POD TARGET: try for up to 60s for the connection to be
# refused or dropped, then require it to STAY that way for three more attempts.
# Any other failure (a DNS error, a missing binary) is not "blocked" and fails.
must_block() {
  local what=$1 from=$2 target=$3 why="" held=0
  for _ in $(seq 1 30); do
    why=$(connect "$from" "$target" "$(container_of "$from")")
    case "$why" in
      REFUSED*|TIMEOUT*) held=$((held + 1)); [ "$held" -ge 4 ] && break;;
      *) held=0;;
    esac
    sleep 2
  done
  if [ "$held" -ge 4 ]; then
    printf '  ok  blocked  %-34s %s -> %s (%s)\n' "$what" "$from" "$target" "$why"; return 0
  fi
  printf '  !!  REACHED  %-34s %s -> %s (last answer: %s)\n' "$what" "$from" "$target" "${why:-connected}"
  return 1
}

run_probe() {
  local name=$1
  kubectl -n "$NS" delete pod "$name" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  kubectl -n "$NS" run "$name" --image="$IMAGE" --image-pull-policy=Never --restart=Never \
    --labels="app=gc-agent" --command -- \
    /usr/local/bin/gonk-intake netpol-probe \
      -allow gonk-meter:8080 \
      -deny   gonk-intake:8080 \
      -deny-l3 gonk-controller:9443 \
      -settle-timeout 45s -dial-timeout 3s >/dev/null
  kubectl -n "$NS" wait --for=jsonpath='{.status.phase}' --timeout=180s pod/"$name" >/dev/null 2>&1 || true
  for _ in $(seq 1 60); do
    phase=$(kubectl -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    case "$phase" in Succeeded|Failed) break;; esac
    sleep 3
  done
  kubectl -n "$NS" logs "$name"
}

log "NEGATIVE CONTROL: no policies applied -- the probe MUST say NOT ENFORCED"
out_before=$(run_probe probe-nopolicy)
echo "$out_before"
if ! grep -q 'VERDICT: NOT ENFORCED' <<<"$out_before"; then
  echo "FAIL: with no NetworkPolicy at all the probe did not report NOT ENFORCED."
  echo "      Either the probe is broken or this cluster is blocking traffic for"
  echo "      some other reason; a green positive case would prove nothing."
  exit 1
fi

log "BASELINE: no policies applied -- every connection below MUST succeed"
fail=0
must_reach "agent fetches its checkout"       agent      "$INTAKE_PRIVATE" || fail=1
must_reach "controller registers a checkout"  controller "$INTAKE_PRIVATE" || fail=1
must_reach "controller calls the API server"  controller "$API_SVC:443"    || fail=1
must_reach "meter calls the API server"       meter      "$API_SVC:443"    || fail=1
if [ "$fail" -ne 0 ]; then
  echo "FAIL: with no NetworkPolicy at all, a connection the test depends on does not work."
  echo "      Nothing below would mean anything."
  exit 1
fi

log "NEGATIVE CONTROL: the v0.2.2 policy shape -- the same connections MUST be blocked"
kubectl -n "$NS" apply -f "$WORK/netpol-v022.yaml"
fail=0
must_block "agent fetches its checkout"       agent      "$INTAKE_PRIVATE" || fail=1
must_block "controller registers a checkout"  controller "$INTAKE_PRIVATE" || fail=1
must_block "controller calls the API server"  controller "$API_SVC:443"    || fail=1
must_block "meter calls the API server"       meter      "$API_SVC:443"    || fail=1
if [ "$fail" -ne 0 ]; then
  echo "FAIL: without the intake ingress rule and the API-server rule, a connection that"
  echo "      depends on them still succeeded. Either this cluster does not enforce ingress"
  echo "      or ipBlock-less egress the way the test assumes, or the control is wrong;"
  echo "      the positive case below would prove nothing."
  exit 1
fi

log "applying the chart's policies"
kubectl -n "$NS" apply -f "$WORK/netpol.yaml"
sleep 10   # let the CNI programme them; the probe also settles on its own

log "POSITIVE CASE: policies applied -- the probe MUST say ENFORCED"
out_after=$(run_probe probe-enforced)
echo "$out_after"
if ! grep -q 'VERDICT: ENFORCED' <<<"$out_after"; then
  echo "FAIL: with the chart's policies applied on a cluster that enforces them,"
  echo "      the probe did not report ENFORCED. Either a policy is wrong or this"
  echo "      cluster is not actually enforcing. That is what this gate is for."
  exit 1
fi

log "POSITIVE CASE: the chart's policies -- what gonk needs MUST be reachable"
fail=0
must_reach "agent fetches its checkout"       agent      "$INTAKE_PRIVATE" || fail=1
must_reach "controller registers a checkout"  controller "$INTAKE_PRIVATE" || fail=1
must_reach "controller calls the API server"  controller "$API_SVC:443"    || fail=1
must_reach "meter calls the API server"       meter      "$API_SVC:443"    || fail=1
if [ "$fail" -ne 0 ]; then
  echo "FAIL: with the chart's policies applied, a connection gonk depends on is blocked."
  echo "      On this cluster no session would get a working copy, or no session Job"
  echo "      would be created at all."
  exit 1
fi

log "AND NOTHING MORE: the new rules admit only the pods they name"
fail=0
must_block "agent calls the API server"       agent      "$API_SVC:443"    || fail=1
must_block "intake calls the API server"      intake     "$API_SVC:443"    || fail=1
must_block "meter reaches intake's private listener" meter "$INTAKE_PRIVATE" || fail=1
if [ "$fail" -ne 0 ]; then
  echo "FAIL: a pod the new rules do not name got through them."
  exit 1
fi

log "PASS: NOT ENFORCED without the policies, ENFORCED with them; the checkout and the API server are reachable with them and were not with the v0.2.2 shape"
