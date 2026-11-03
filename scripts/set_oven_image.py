#!/usr/bin/env python3
"""릴리스 CI용: values.yaml의 oven.image.tag·digest를 채운다. 주석을 살리려고 YAML을 다시 쓰지 않고 그 두 줄만 바꾼다."""
import pathlib
import re
import sys

VALUES = pathlib.Path(__file__).resolve().parents[1] / "charts" / "kukie-gpu" / "values.yaml"


def set_image(text, tag, digest):
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ValueError(f"digest 형식이 아닙니다: {digest}")
    head, sep, block = text.partition("\noven:\n")
    if not sep:
        raise ValueError("values.yaml에 oven 블록이 없습니다")
    block, tags = re.subn(r'^    tag: ""$', f'    tag: "{tag}"', block, count=1, flags=re.M)
    block, digests = re.subn(r'^    digest: ""$', f'    digest: "{digest}"', block, count=1, flags=re.M)
    if tags != 1 or digests != 1:
        raise ValueError('oven.image에서 tag: ""와 digest: "" 줄을 찾지 못했습니다')
    return head + sep + block


def main():
    tag, digest = sys.argv[1:]
    VALUES.write_text(set_image(VALUES.read_text(encoding="utf-8"), tag, digest), encoding="utf-8")


if __name__ == "__main__":
    main()
