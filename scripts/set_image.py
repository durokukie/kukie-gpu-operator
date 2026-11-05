#!/usr/bin/env python3
"""릴리스 CI용: values.yaml의 <블록>.image.tag·digest를 채운다. 주석을 살리려고 YAML을 다시 쓰지 않고 그 두 줄만 바꾼다.

  python3 scripts/set_image.py oven 0.7.0 sha256:...
  python3 scripts/set_image.py counter 0.7.0 sha256:...
"""
import pathlib
import re
import sys

VALUES = pathlib.Path(__file__).resolve().parents[1] / "charts" / "kukie-gpu" / "values.yaml"


def set_image(text, block_name, tag, digest):
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError(f"digest 형식이 아닙니다: {digest}")
    start = re.search(rf"^{re.escape(block_name)}:\n", text, flags=re.M)
    if not start:
        raise ValueError(f"values.yaml에 {block_name} 블록이 없습니다")
    # 다음 최상위 줄 앞까지만 본다. 이 블록이 이미 채워졌을 때 다음 블록의 빈 줄을 대신 채우면 안 된다.
    end = re.compile(r"^\S", flags=re.M).search(text, start.end())
    stop = end.start() if end else len(text)
    block = text[start.end():stop]
    block, tags = re.subn(r'^    tag: ""$', f'    tag: "{tag}"', block, count=1, flags=re.M)
    block, digests = re.subn(r'^    digest: ""$', f'    digest: "{digest}"', block, count=1, flags=re.M)
    if tags != 1 or digests != 1:
        raise ValueError(f'{block_name}.image에서 tag: ""와 digest: "" 줄을 찾지 못했습니다')
    return text[:start.end()] + block + text[stop:]


def main():
    block_name, tag, digest = sys.argv[1:]
    VALUES.write_text(set_image(VALUES.read_text(encoding="utf-8"), block_name, tag, digest), encoding="utf-8")


if __name__ == "__main__":
    main()
