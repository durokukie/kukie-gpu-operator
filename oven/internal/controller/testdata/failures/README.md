# 실패 로그 고정 입력 (DURO-171)

`failure_test.go`가 실패 원인 패턴표(`../../failure_patterns.yaml`)를 이 파일들로 검사한다. 로그를 고칠 때는 어디서 얻었는지도 여기 고친다.

| 파일 | 출처 |
| -- | -- |
| `hf-gated.log` | 2026-11-11 `hf download meta-llama/Llama-3.1-8B-Instruct config.json`, 토큰 없음, hf CLI 2.4.0의 실제 출력 |
| `hf-gated-debug.log` | 같은 명령에 `HF_DEBUG=1`. 예외 이름(`GatedRepoError`)과 401이 나오는 실제 출력 |
| `hf-not-found.log` | 2026-11-11 없는 저장소를 받은 실제 출력. 비공개 저장소도 토큰 없이는 같은 답이다 |
| `hf-disk-full.log` | 2026-11-11 4MB 디스크 이미지에 11MB 파일을 받은 실제 출력(`os error 28`) |
| `vllm-mamba-max-num-seqs.log` | 2026-10-25 Qwen3.8-27B-FP8이 L40S에서 기본 `max_num_seqs` 256으로 멈춘 일(Mamba 캐시 185). 원문 로그는 남지 않아 vLLM `vllm/config/compilation.py`의 오류 문구에 그때 숫자를 넣었다 |
| `vllm-kv-cache-short.log` | vLLM `vllm/v1/core/kv_cache_planning.py`의 오류 문구. 실제 GPU에서 받은 로그가 아니다 |
| `vllm-unsupported-arch.log` | vLLM `vllm/model_executor/models/registry.py`의 오류 문구. 실제 GPU에서 받은 로그가 아니다 |
| `vllm-cuda-oom.log` | PyTorch `torch.OutOfMemoryError` 문구. 실제 GPU에서 받은 로그가 아니다 |

로컬 경로는 Pod 안 경로처럼 바꿨다(`/usr/local/lib/python3.12/dist-packages`, `/models`). 실제 GPU에서 실패를 재현하면 vLLM 쪽 세 파일을
그 로그로 바꾼다.
