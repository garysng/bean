#!/usr/bin/env python3
"""Reports where the LSMT and ZFile headers sit inside a sealed overlaybd layer.

bean seals every layer with `overlaybd-commit -z -t`, and -t wraps the result in a tar so
it is a valid OCI blob. That wrapper means the LSMT header is not at offset 0, which is
what a reader written against the bare format assumes. This prints the real offsets so
the reader can be corrected against them rather than against a guess.
"""

import sys

LSMT_MAGIC = bytes.fromhex("4c534d5400010200")
ZFILE_MAGIC = bytes.fromhex("5a46696c65000100")
TAR_BLOCK = 512


def octal(field: bytes) -> int:
    """Parse a tar numeric field, which is octal ASCII with junk after it."""
    text = field.split(b"\0")[0].strip()
    return int(text, 8) if text else 0


def main(path: str) -> None:
    data = open(path, "rb").read()
    print(f"file: {path}")
    print(f"size: {len(data)}")

    print(f"first 8 bytes: {data[:8].hex()}")
    print(f"LSMT magic at:  {data.find(LSMT_MAGIC)}")
    print(f"ZFile magic at: {data.find(ZFILE_MAGIC)}")

    # Walk the tar entries. Named fields rather than a library so the offsets printed are
    # the ones a Go reader would have to compute.
    off = 0
    while off + TAR_BLOCK <= len(data):
        header = data[off:off + TAR_BLOCK]
        if header == b"\0" * TAR_BLOCK:
            print(f"@{off}: end-of-archive block")
            break
        name = header[0:100].split(b"\0")[0].decode(errors="replace")
        if not name:
            break
        size = octal(header[124:136])
        typeflag = chr(header[156]) if header[156] else "0"
        body = off + TAR_BLOCK
        print(f"@{off}: entry name={name!r} type={typeflag} size={size} body_at={body}")
        if size:
            print(f"    body first 8: {data[body:body + 8].hex()}")
        # Bodies are padded to a 512 multiple.
        off = body + ((size + TAR_BLOCK - 1) // TAR_BLOCK) * TAR_BLOCK


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "/tmp/obd-seal-probe/sealed.lsmt")
