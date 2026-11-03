#!/bin/sh
# kukie-gpu preflight: runs once after `helm install`/`helm upgrade` (post-install/post-upgrade hook Job) and writes
# what the cluster is missing to the ConfigMap kukie-gpu-preflight (data.result.json). A failed check never fails the
# Job: the install goes through and the Kukie screen shows the list. Mounted from the ConfigMap
# kukie-gpu-preflight-script; environment comes from the Job spec (templates/preflight.yaml).
#
# Result shape:
#   {"checkedAt": "...", "summary": "4/5 통과", "checks": [{"name", "ok", "status", "detail", "hint"}]}
#   ok: true (pass) | false (fail) | null (skip: could not or need not run). status says the same in words.
#
# Time budget: every wait below is bounded so the worst case (DCGM_WAIT + 10 s + PVC_WAIT + two PVC deletes of at
# most 30 s) stays under the Job's activeDeadlineSeconds, which in turn stays under helm's default --timeout (5m).
# If the Pod is killed anyway (deadline, eviction, next upgrade), the EXIT trap deletes the test PVC and writes the
# checks done so far, marked as incomplete.
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
PERMISSION_SA="${PERMISSION_SERVICE_ACCOUNT:-}"
POD_NAME="${POD_NAME:-}"
POD_UID="${POD_UID:-}"

CHECKS=""
PASSED=0
TOTAL=0
GPU_NODES=0
GPU_TOTAL=0
DONE=0

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
    # No Running Pod yet: tell "still pulling the 1 GB image" (normal on a first install) apart from a Pod that
    # cannot start at all.
    states=$(kubectl -n "$DCGM_NS" get pods -l app.kubernetes.io/name=dcgm-exporter \
      -o jsonpath='{range .items[*]}{.status.phase}/{.status.containerStatuses[0].state.waiting.reason}{" "}{end}' 2>/dev/null)
    look="kubectl -n $DCGM_NS get pod -l app.kubernetes.io/name=dcgm-exporter"
    case "$states" in
      "") add_check driver fail "DCGM exporter Pod가 없음 (namespace $DCGM_NS)" \
            "GPU 노드가 gpuNode.nodeSelector·tolerations와 맞는지 확인: $look -o wide" ;;
      *ImagePull*|*ErrImage*) add_check driver fail "DCGM exporter 이미지를 못 받음 ($states)" \
            "레지스트리 접속 확인 또는 image.registry로 사설 미러 지정: $look" ;;
      *CrashLoop*|*Error*|*Failed*) add_check driver fail "DCGM exporter Pod가 뜨지 못함 ($states)" \
            "노드 이미지에 NVIDIA 드라이버가 있는지 확인(kubernetes/nvidia/check.yaml의 nvidia-smi Pod): $look" ;;
      *) add_check driver skip "DCGM exporter Pod가 아직 뜨는 중 ($states) — ${DCGM_WAIT}초 안에 Running이 되지 않음" \
            "Pod가 뜬 뒤 helm upgrade로 다시 실행: $look" ;;
    esac
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

# Delete the test PVC and wait at most 30 s for it to go (a leftover from a killed run is deleted the same way).
delete_pvc() {
  kubectl -n "$RELEASE_NS" delete pvc "$PVC_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  i=0
  while [ "$i" -lt 6 ] && kubectl -n "$RELEASE_NS" get pvc "$PVC_NAME" >/dev/null 2>&1; do sleep 5; i=$((i + 1)); done
}

# The test PVC is owned by this Pod, so the garbage collector removes it (and its volume) even if the script never
# reaches delete_pvc: the Pod goes with the Job (hook-delete-policy before-hook-creation, ttlSecondsAfterFinished).
pvc_manifest() {
  printf 'apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: %s\n  namespace: %s\n  labels:\n    app.kubernetes.io/name: kukie-gpu-preflight\n' "$PVC_NAME" "$RELEASE_NS"
  if [ -n "$POD_NAME" ] && [ -n "$POD_UID" ]; then
    printf '  ownerReferences:\n    - apiVersion: v1\n      kind: Pod\n      name: %s\n      uid: %s\n' "$POD_NAME" "$POD_UID"
  fi
  printf 'spec:\n  accessModes: [ReadWriteOnce]\n  storageClassName: %s\n  resources:\n    requests:\n      storage: %s\n' "$1" "$PVC_SIZE"
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

  if [ "$mode" = WaitForFirstConsumer ]; then
    # Binds only when a Pod mounts it, so a PVC alone cannot show whether the provisioner works. Report what was
    # checked (StorageClass and CSI driver exist) without counting it as a pass.
    add_check storage skip "StorageClass ${sc}(${provisioner})와 CSI 드라이버는 있음. WaitForFirstConsumer라 Pod가 붙어야 볼륨이 만들어져 바인딩까지는 확인하지 않음" \
      "3층 PVC가 Pending으로 남으면 CSI 컨트롤러 로그·IAM(IRSA) 권한 확인"
    return
  fi
  if kubectl -n "$RELEASE_NS" get pvc "$PVC_NAME" >/dev/null 2>&1; then delete_pvc; fi
  if ! out=$(pvc_manifest "$sc" | kubectl create --validate=false -f - 2>&1); then
    add_check storage fail "테스트 PVC 생성 실패: $out" "스토리지 쿼터·어드미션 정책 확인"
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

# 5. Permissions: can the ServiceAccount that layers 2-3 will run as (preflight.permission.serviceAccount) create
#    Deployment, Job and PVC? SubjectAccessReview, so no impersonation. Skipped until that ServiceAccount exists:
#    this Job's own ServiceAccount is deliberately too small to be a meaningful subject.
# --validate=false: client-side validation would try to list CRDs, which this ServiceAccount may not.
sar_allowed() {
  printf 'apiVersion: authorization.k8s.io/v1\nkind: SubjectAccessReview\nspec:\n  user: system:serviceaccount:%s:%s\n  groups: [system:serviceaccounts, system:serviceaccounts:%s, system:authenticated]\n  resourceAttributes:\n    namespace: %s\n    verb: create\n    group: %s\n    resource: %s\n' \
    "$PERMISSION_NS" "$PERMISSION_SA" "$PERMISSION_NS" "$PERMISSION_NS" "$1" "$2" \
    | kubectl create --validate=false -f - -o jsonpath='{.status.allowed}' 2>/tmp/sar.err
}

check_permission() {
  if [ -z "$PERMISSION_SA" ]; then
    add_check permission skip "검사할 ServiceAccount가 지정되지 않아 건너뜀 (1층은 생성 권한이 필요 없음)" \
      "2층 측정 Job·3층 ServedModel을 쓸 때 preflight.permission.serviceAccount에 그 ServiceAccount 이름을 지정"
    return
  fi
  allowed=""
  denied=""
  unknown=""
  why=""
  for item in apps/deployments batch/jobs /persistentvolumeclaims; do
    # Only an answer from the API server counts. A failed SubjectAccessReview says nothing about the ServiceAccount.
    answer=$(sar_allowed "${item%/*}" "${item#*/}") && rc=0 || rc=$?
    if [ "$rc" = 0 ] && [ "$answer" = true ]; then
      allowed="${allowed:+$allowed, }${item#*/}"
    elif [ "$rc" = 0 ] && [ "$answer" = false ]; then
      denied="${denied:+$denied, }${item#*/}"
    else
      unknown="${unknown:+$unknown, }${item#*/}"
      why=$(head -n 1 /tmp/sar.err 2>/dev/null | cut -c1-200)
    fi
  done
  subject="ServiceAccount ${PERMISSION_NS}/${PERMISSION_SA}"
  if [ -n "$denied" ]; then
    add_check permission fail "${subject}: 생성 불가 ${denied}${allowed:+ (가능: $allowed)}${unknown:+ (확인 못 함: $unknown)}" \
      "그 ServiceAccount에 namespace ${PERMISSION_NS}의 Deployment·Job·PVC create RBAC를 준다"
  elif [ -n "$unknown" ]; then
    add_check permission skip "${subject}: 권한을 확인하지 못함 (${unknown})${why:+: $why}" \
      "이 Job의 SubjectAccessReview 생성이 실패했다. 권한 부족이라는 뜻이 아니다. Job 로그와 kukie-gpu-preflight ServiceAccount의 권한을 확인한다"
  else
    add_check permission pass "${subject}: 생성 가능 ${allowed}" ""
  fi
}

write_result() {
  checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  summary="${PASSED}/${TOTAL} 통과"
  [ "$DONE" = 1 ] || summary="${summary} (중단됨: 검사가 끝나기 전에 Job이 종료됨)"
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

# Whatever happens, remove the test PVC and record what was checked so far. TERM (deadline, eviction) is turned into
# an exit so the EXIT trap runs; the result then says it was cut short.
on_exit() {
  trap - EXIT
  delete_pvc
  write_result
}
trap on_exit EXIT
trap 'exit 143' TERM INT

check_gpu
check_driver
check_huggingface
check_storage
check_permission
DONE=1
exit 0
