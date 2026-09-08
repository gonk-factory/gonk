#!/usr/bin/env bash
# Prove the gonk-agent egress policy is CORRECT, on a cluster that enforces it.
#
# WHY THIS CANNOT RUN ANYWHERE ELSE (gonk-v47). orac runs flannel with no policy
# controller, so every NetworkPolicy in this chart is accepted by the API server
# and enforced by nothing (gonk-dku, now measurable with `helm test gonk`). The
# GitLab runner cannot help either: its k8s executor is privileged=false, so no
# kind, no k3s, no DinD. That leaves a GitHub runner, which has real Docker.
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
set -euo pipefail

NS=${NS:-gonk-netpol-e2e}
IMAGE=${IMAGE:?set IMAGE to the gonk-intake image loaded into the cluster}
# agnhost is the upstream Kubernetes network-test image; netexec serves HTTP on
# a port and does nothing else. Pinned: a floating tag here would silently
# change what "the destination" is.
AGNHOST=${AGNHOST:-registry.k8s.io/e2e-test-images/agnhost:2.53}

log() { printf '\n=== %s\n' "$*"; }

log "namespace $NS"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

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
    # ONE POD, TWO PORTS. 9090 is permitted by the agent policy and 8080 is not,
    # so the probe's two legs differ only in the port the policy names.
    - name: public
      image: $AGNHOST
      args: ["netexec", "--http-port=8080"]
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
metadata: {name: gonk-controller}
spec:
  selector: {app.kubernetes.io/component: controller}
  ports: [{port: 9443, targetPort: 9443}]
EOF
kubectl -n "$NS" wait --for=condition=Ready pod/intake pod/meter pod/controller --timeout=180s

# The policies straight out of the chart -- not hand-written here, or this would
# test a copy rather than what ships.
log "rendering the chart's NetworkPolicies"
# ALL of them, not just the agent's. NetworkPolicy is enforced on EGRESS AT THE
# SOURCE and on INGRESS AT THE DESTINATION, so applying only the agent policy
# would leave every destination's default-deny ingress in force and the allow leg
# would fail for a reason that has nothing to do with what is being tested.
NS="$NS" python3 - > /tmp/netpol.yaml <<'PYEOF'
import os, subprocess, yaml
out = subprocess.run(
    ["helm", "template", "gonk", "chart/gonk", "--namespace", os.environ["NS"],
     "--values", "chart/gonk/ci/values-default.yaml"],
    capture_output=True, text=True, check=True).stdout
docs = [d for d in yaml.safe_load_all(out) if d and d.get("kind") == "NetworkPolicy"]
print(yaml.safe_dump_all(docs), end="")
PYEOF
grep -c 'kind: NetworkPolicy' /tmp/netpol.yaml

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

log "applying the chart's policies"
kubectl -n "$NS" apply -f /tmp/netpol.yaml
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

log "PASS: NOT ENFORCED without the policies, ENFORCED with them"
