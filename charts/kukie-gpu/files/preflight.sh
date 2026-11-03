#!/bin/sh
# kukie-gpu preflight: runs once after `helm install`/`helm upgrade` (post-install/post-upgrade hook Job) and writes
# what the cluster is missing to the ConfigMap kukie-gpu-preflight (data.result.json). A failed check never fails the
# Job: the install goes through and the Kukie screen shows the list. Mounted from the ConfigMap
# kukie-gpu-preflight-script; environment comes from the Job spec (templates/preflight.yaml).
#
# Result shape:
#   {"checkedAt": "...", "summary": "4/5 통과", "checks": [{"name", "ok", "status", "detail", "hint"}]}
#   ok: true (pass) | false (fail) | null (skip: could not or need not run). status says the same in words.
set -u

RELEASE_NS="${RELEASE_NAMESPACE:?}"
RESULT_CM="${RESULT_CONFIGMAP:-kukie-gpu-preflight}"
DCGM_NS="${DCGM_NAMESPACE:?}"
DCGM_ENABLED="${DCGM_ENABLED:-true}"
DCGM_WAIT="${DCGM_WAIT_SECONDS:-60}"
HF_URL="${HF_URL:-https://huggingface.co/api/models?limit=1}"
PVC_NAME="${PVC_NAME:-kukie-gpu-preflight-test}"
PVC_SIZE="${PVC_SIZE:-1Gi}"
PVC_WAIT="${PVC_WAIT_SECONDS:-60}"
SC_NAME="${STORAGE_CLASS_NAME:-}"
PERMISSION_NS="${PERMISSION_NAMESPACE:-$RELEASE_NS}"

CHECKS=""
PASSED=0
TOTAL=0
GPU_NODES=0
GPU_TOTAL=0

# JSON string escaping for sh: backslash, double quote, tab and newline. Control characters other than these do not
# occur in what we write (kubectl/curl output is trimmed to one line).
json_str() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\t/\\t/g' | tr '\n' ' ' | sed -e 's/ $//'
}

# add_check NAME STATUS DETAIL HINT   (STATUS: pass | fail | skip)
add_check() {
  name="$1" status="$2" detail="$3" hint="$4"
  case "$status" in
    pass) ok=true; PASSED=$((PASSED + 1)); TOTAL=$((TOTAL + 1)) ;;
    fail) ok=false; TOTAL=$((TOTAL + 1)) ;;
    *) ok=null ;;
  esac
  item="{\"name\":\"$(json_str "$name")\",\"ok\":$ok,\"status\":\"$status\",\"detail\":\"$(json_str "$detail")\",\"hint\":\"$(json_str "$hint")\"}"
  CHECKS="${CHECKS:+$CHECKS,}$item"
  printf '[%s] %s: %s\n' "$status" "$name" "$detail"
  [ -n "$hint" ] && [ "$status" != pass ] && printf '        -> %s\n' "$hint"
  return 0
}

# 1. GPU resource: any node with allocatable nvidia.com/gpu > 0
check_gpu() {
  if ! out=$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' 2>&1); then
    add_check gpu fail "노드 목록을 읽지 못함: $out" "preflight ServiceAccount의 ClusterRole(nodes list) 확인"
    return
  fi
  nodes=0
  for line in $out; do
    nodes=$((nodes + 1))
    count="${line#*=}"
    case "$count" in ''|*[!0-9]*) continue ;; esac
    if [ "$count" -gt 0 ]; then
      GPU_NODES=$((GPU_NODES + 1))
      GPU_TOTAL=$((GPU_TOTAL + count))
    fi
  done
  if [ "$GPU_NODES" -gt 0 ]; then
    add_check gpu pass "GPU 노드 ${GPU_NODES}대, nvidia.com/gpu 합계 ${GPU_TOTAL}" ""
  else
    add_check gpu fail "nvidia.com/gpu 자원이 있는 노드가 없음 (노드 ${nodes}대)" \
      "GPU 노드를 켜거나, 장치 플러그인이 없으면 devicePlugin.enabled=true로 설치"
  fi
}

# 2. Driver: read DCGM_FI_DRIVER_VERSION from a running DCGM exporter Pod's :9400/metrics (no GPU Pod of our own).
check_driver() {
  if [ "$GPU_NODES" -eq 0 ]; then
    add_check driver skip "GPU 노드가 없어 건너뜀" "GPU 노드가 생기면 helm upgrade로 다시 실행"
    return
  fi
  if [ "$DCGM_ENABLED" != true ]; then
    add_check driver skip "dcgm.enabled=false라 드라이버 버전을 읽을 DCGM exporter가 없음" \
      "dcgm.enabled=true로 설치하거나 노드에서 nvidia-smi로 직접 확인"
    return
  fi
  ip=""
  waited=0
  while [ "$waited" -le "$DCGM_WAIT" ]; do
    ip=$(kubectl -n "$DCGM_NS" get pods -l app.kubernetes.io/name=dcgm-exporter \
      -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.status.podIP}{"\n"}{end}' 2>/dev/null | head -1)
    [ -n "$ip" ] && break
    sleep 5
    waited=$((waited + 5))
  done
  if [ -z "$ip" ]; then
    add_check driver fail "DCGM exporter Pod가 ${DCGM_WAIT}초 안에 Running이 되지 않음 (namespace $DCGM_NS)" \
      "kubectl -n $DCGM_NS get pod -l app.kubernetes.io/name=dcgm-exporter 로 상태 확인. 드라이버가 없는 노드 이미지면 Pod가 못 뜬다"
    return
  fi
  metrics=$(curl -sS --max-time 10 "http://$ip:9400/metrics" 2>&1)
  version=$(printf '%s\n' "$metrics" | grep -o 'DCGM_FI_DRIVER_VERSION="[^"]*"' | head -1 | cut -d'"' -f2)
  if [ -n "$version" ]; then
    add_check driver pass "NVIDIA 드라이버 ${version} (DCGM exporter $ip)" ""
  else
    add_check driver fail "DCGM exporter($ip:9400)에서 드라이버 버전을 읽지 못함: $(printf '%s' "$metrics" | head -c 200)" \
      "노드 이미지에 NVIDIA 드라이버가 있는지 확인(kubernetes/nvidia/check.yaml의 nvidia-smi Pod)"
  fi
}

# 3. Hugging Face reachability: GET must answer 200 within 10 s
check_huggingface() {
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "$HF_URL" 2>/tmp/curl.err) || true
  if [ "$code" = 200 ]; then
    add_check huggingface pass "GET $HF_URL -> 200" ""
  else
    err=$(head -c 200 /tmp/curl.err 2>/dev/null)
    add_check huggingface fail "GET $HF_URL -> ${code:-없음} ${err}" \
      "프록시(HTTPS_PROXY)를 설정하거나 모델 출처를 S3·PVC로 바꾼다"
  fi
}

# 4. Storage: the StorageClass exists and its CSI driver is installed; a test PVC binds (Immediate) or at least gets
#    created (WaitForFirstConsumer binds only when a Pod mounts it).
csi_driver_for() {
  case "$1" in
    kubernetes.io/aws-ebs) echo ebs.csi.aws.com ;;
    kubernetes.io/gce-pd) echo pd.csi.storage.gke.io ;;
    kubernetes.io/azure-disk) echo disk.csi.azure.com ;;
    *csi*) echo "$1" ;;
    *) echo "" ;;
  esac
}

pvc_phase() { kubectl -n "$RELEASE_NS" get pvc "$PVC_NAME" -o jsonpath='{.status.phase}' 2>/dev/null; }

delete_pvc() {
  kubectl -n "$RELEASE_NS" delete pvc "$PVC_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  i=0
  while [ "$i" -lt 12 ] && kubectl -n "$RELEASE_NS" get pvc "$PVC_NAME" >/dev/null 2>&1; do sleep 5; i=$((i + 1)); done
}

check_storage() {
  if [ -n "$SC_NAME" ]; then
    sc="$SC_NAME"
    if ! kubectl get storageclass "$sc" >/dev/null 2>&1; then
      add_check storage fail "StorageClass ${sc}이(가) 없음" "preflight.storage.storageClassName을 있는 이름으로 지정"
      return
    fi
  else
    sc=$(kubectl get storageclass -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1)
    if [ -z "$sc" ]; then
      add_check storage fail "기본 StorageClass가 없음" "preflight.storage.storageClassName 지정 (3층은 ServedModel.spec.storage.storageClassName)"
      return
    fi
  fi
  provisioner=$(kubectl get storageclass "$sc" -o jsonpath='{.provisioner}' 2>/dev/null)
  mode=$(kubectl get storageclass "$sc" -o jsonpath='{.volumeBindingMode}' 2>/dev/null)
  driver=$(csi_driver_for "$provisioner")
  if [ -n "$driver" ] && ! kubectl get csidriver "$driver" >/dev/null 2>&1; then
    add_check storage fail "StorageClass ${sc}(${provisioner})의 CSI 드라이버 ${driver}가 설치돼 있지 않음 — PVC가 영원히 Pending" \
      "CSI 드라이버를 설치(EKS면 aws-ebs-csi-driver 애드온)하거나 다른 storageClassName 지정"
    return
  fi

  delete_pvc
  if ! out=$(printf 'apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n  namespace: %s\n  labels:\n    app.kubernetes.io/name: kukie-gpu-preflight\nspec:\n  accessModes: [ReadWriteOnce]\n  storageClassName: %s\n  resources:\n    requests:\n      storage: %s\n' \
      "$PVC_NAME" "$RELEASE_NS" "$sc" "$PVC_SIZE" | kubectl create -f - 2>&1); then
    add_check storage fail "테스트 PVC 생성 실패: $out" "스토리지 쿼터·어드미션 정책 확인"
    return
  fi
  if [ "$mode" = WaitForFirstConsumer ]; then
    # Binds only when a Pod mounts it, so waiting proves nothing; the StorageClass/CSI checks above are the test.
    phase=$(pvc_phase)
    delete_pvc
    add_check storage pass "StorageClass ${sc}(${provisioner}, WaitForFirstConsumer): 테스트 PVC 생성됨(${phase:-Pending}). 볼륨은 첫 Pod가 붙을 때 만들어진다" ""
    return
  fi
  phase=""
  waited=0
  while [ "$waited" -le "$PVC_WAIT" ]; do
    phase=$(pvc_phase)
    [ "$phase" = Bound ] && break
    sleep 5
    waited=$((waited + 5))
  done
  delete_pvc
  if [ "$phase" = Bound ]; then
    add_check storage pass "StorageClass ${sc}(${provisioner}): 테스트 PVC ${PVC_SIZE} Bound" ""
  else
    add_check storage fail "StorageClass ${sc}(${provisioner}): 테스트 PVC가 ${PVC_WAIT}초 안에 Bound가 아님 (${phase:-없음})" \
      "프로비저너 로그를 보거나 다른 storageClassName 지정"
  fi
}

# 5. Permissions: can this ServiceAccount create Deployment, Job and PVC (what layers 2-3 need)? Informational for
#    layer 1: its own RBAC is deliberately smaller than that.
check_permission() {
  allowed=""
  denied=""
  for res in deployments.apps jobs.batch persistentvolumeclaims; do
    if kubectl auth can-i create "$res" -n "$PERMISSION_NS" >/dev/null 2>&1; then
      allowed="${allowed:+$allowed, }$res"
    else
      denied="${denied:+$denied, }$res"
    fi
  done
  if [ -z "$denied" ]; then
    add_check permission pass "namespace ${PERMISSION_NS}에서 생성 가능: ${allowed}" ""
  else
    add_check permission fail "namespace ${PERMISSION_NS}에서 생성 불가: ${denied}${allowed:+ (가능: $allowed)}" \
      "1층(관측)만 쓰면 무시. 2층 측정 Job·3층 ServedModel을 쓰려면 그 ServiceAccount에 RBAC를 준다"
  fi
}

write_result() {
  checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  summary="${PASSED}/${TOTAL} 통과"
  {
    printf 'data:\n  checkedAt: "%s"\n  summary: "%s"\n  result.json: |\n' "$checked_at" "$summary"
    printf '    {"checkedAt":"%s","summary":"%s","checks":[%s]}\n' "$checked_at" "$summary" "$CHECKS"
  } > /tmp/patch.yaml
  if kubectl -n "$RELEASE_NS" patch configmap "$RESULT_CM" --type merge --patch-file /tmp/patch.yaml >/dev/null; then
    printf '\n%s -> ConfigMap %s/%s\n' "$summary" "$RELEASE_NS" "$RESULT_CM"
  else
    printf '\n결과를 ConfigMap %s/%s에 쓰지 못했습니다. 위 로그가 결과입니다.\n' "$RELEASE_NS" "$RESULT_CM" >&2
  fi
}

check_gpu
check_driver
check_huggingface
check_storage
check_permission
write_result
exit 0
