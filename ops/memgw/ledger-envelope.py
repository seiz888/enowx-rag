#!/usr/bin/env python3
"""Envelope encryption for the memgw ledger backup.

Why this exists
---------------
A backup that can only be read on the machine that made it is not a backup: it
protects against a corrupt disk, not against losing the machine. The original
ledger backup was sealed with DPAPI, whose key is derived from the Windows
account and never leaves it.

Format
------
v2 (`MEMGWSE2`) is a streaming authenticated envelope:

  * a fresh random 256-bit data key encrypts the payload with **AES-256-GCM**
  * the data key is wrapped with RSA-OAEP (SHA-256) to a recovery public key
    whose private half is escrowed outside this machine
  * the payload is split into 1 MiB segments, each independently authenticated,
    with the tag of segment i-1 mixed into segment i's associated data, and a
    final-segment flag authenticated in the AAD

Two properties are load-bearing and were the reason for replacing v1:

**Every byte is authenticated before it is written.** v1 was AES-256-CBC with a
PKCS7 pad and a sha256 sidecar. CBC alone detects nothing: a flipped ciphertext
byte corrupts one block and usually still unpads, and the sha256 sidecar is a
separate file that an attacker who can modify the payload can also modify. v1
therefore could not distinguish a tampered backup from a good one, and its
`unseal` wrote plaintext before any integrity check existed. If the v1 file is
corrupt, CBC can also emit plaintext that passes padding and only *looks* fine.

**Truncation is detected.** Because the last segment is marked inside its own
authenticated data, dropping trailing segments makes the new last segment fail
its tag check rather than silently restoring a short dump.

The tag chaining also authenticates the header (it is segment 0's associated
data), so the nonce, the chunk size and the recorded fingerprint cannot be
edited without the first segment failing.

No secret is printed. `seal --key-out` writes the raw data key to a file for the
caller to wrap with DPAPI; it is never written to stdout, a log or a transcript.

Subcommands
-----------
  seal             encrypt a payload and wrap the data key to the recovery key
  unwrap-recovery  unwrap the data key with the recovery private key
  unseal           authenticate and decrypt the payload with a data key
  verify           authenticate the payload without writing any plaintext
  fingerprint      print sha256 of a public key's SubjectPublicKeyInfo

v1 (`MEMGW1`, AES-CBC + separate .iv) is still readable through `--legacy` so an
existing artefact can be recovered. It is archival recovery only: it is never
written by this tool, and it must not be used for a new backup.
"""

import argparse
import base64
import hashlib
import json
import os
import sys
import time

from cryptography.exceptions import InvalidTag
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.padding import PKCS7

# v2 magic. Eight bytes so the version is legible in a hexdump and cannot be
# confused with v1, which had no magic at all (v1 is detected by its absence).
MAGIC = b"MEMGWSE2"
CHUNK = 1 << 20
TAG_LEN = 16
# 8 random bytes plus a 4-byte counter is the 96-bit nonce AES-GCM wants, with
# the counter making each segment's nonce distinct under one key.
NONCE_PREFIX_LEN = 8
MAX_CHUNKS = 1 << 32

OAEP = padding.OAEP(mgf=padding.MGF1(algorithm=hashes.SHA256()),
                    algorithm=hashes.SHA256(), label=None)


def read_key(path):
    with open(path, "rb") as fh:
        return fh.read()


def load_public(path):
    with open(path, "rb") as fh:
        return serialization.load_pem_public_key(fh.read())


def load_private(path):
    with open(path, "rb") as fh:
        return serialization.load_pem_private_key(fh.read(), password=None)


def spki_fingerprint(pub):
    der = pub.public_bytes(serialization.Encoding.DER,
                           serialization.PublicFormat.SubjectPublicKeyInfo)
    return hashlib.sha256(der).hexdigest()


def cmd_fingerprint(args):
    print(spki_fingerprint(load_public(args.public)))
    return 0


def read_exactly(src, n):
    """Read exactly n bytes or raise. A short read is a truncated file, and
    treating it as "the rest of the data" is how a partial backup gets read as a
    whole one."""
    buf = src.read(n)
    if len(buf) != n:
        raise SystemExit("truncated envelope: wanted %d bytes, got %d" % (n, len(buf)))
    return buf


def write_file_0600(path, data):
    """Write with owner-only permissions where the platform honours them.

    On Windows the mode is largely advisory, so the caller must also delete the
    file promptly; this is the second half of keeping a data key off every
    shared surface, not the whole of it."""
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "wb") as fh:
        fh.write(data)
        fh.flush()
        os.fsync(fh.fileno())


def cmd_seal(args):
    if getattr(args, "legacy", False):
        raise SystemExit("refusing: this tool does not write the v1 format; "
                         "v1 is readable for archival recovery only")

    key = os.urandom(32)
    nonce_prefix = os.urandom(NONCE_PREFIX_LEN)
    pub = load_public(args.recovery_public)
    wrapped = pub.encrypt(key, OAEP)
    fingerprint = spki_fingerprint(pub)

    plaintext_bytes = os.path.getsize(args.input)
    header = json.dumps({
        "v": 2,
        "cipher": "AES-256-GCM",
        "segment": "chained-tag, final-flag in AAD",
        "chunk": CHUNK,
        "nonce_prefix": nonce_prefix.hex(),
        "recovery_key": "rsa-4096-oaep-sha256",
        "recovery_fingerprint": fingerprint,
        "plaintext_bytes": plaintext_bytes,
        "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    }, sort_keys=True, separators=(",", ":")).encode("ascii")

    aesgcm = AESGCM(key)
    segments = 0
    written = 0
    prev_tag = b"\x00" * TAG_LEN

    with open(args.input, "rb") as src, open(args.out_prefix + ".aes", "wb") as dst:
        dst.write(MAGIC)
        dst.write(len(header).to_bytes(4, "big"))
        dst.write(header)

        cur = src.read(CHUNK)
        while True:
            nxt = src.read(CHUNK)
            is_last = (nxt == b"")
            aad = header + prev_tag + (b"\x01" if is_last else b"\x00")
            if segments >= MAX_CHUNKS:
                raise SystemExit("payload exceeds the nonce counter space")
            nonce = nonce_prefix + segments.to_bytes(4, "big")
            ct = aesgcm.encrypt(nonce, cur, aad)
            prev_tag = ct[-TAG_LEN:]
            dst.write(len(ct).to_bytes(4, "big"))
            dst.write(ct)
            written += len(cur)
            segments += 1
            if is_last:
                break
            cur = nxt

    with open(args.out_prefix + ".key.recovery", "wb") as fh:
        fh.write(wrapped)
    write_file_0600(args.out_prefix + ".key.recovery",
                    wrapped)
    with open(args.out_prefix + ".key.recovery.fingerprint", "w", encoding="ascii") as fh:
        fh.write(fingerprint + "\n")

    if args.key_out:
        # The caller needs the raw key only to wrap it a second time (DPAPI).
        # It goes to a file, never to stdout: stdout is what ends up in a log.
        write_file_0600(args.key_out, key)

    if args.verbose:
        print("plaintext_bytes=%d" % written, file=sys.stderr)
        print("ciphertext_bytes=%d" % os.path.getsize(args.out_prefix + ".aes"),
              file=sys.stderr)
        print("segments=%d" % segments, file=sys.stderr)
        print("recovery_fingerprint=%s" % fingerprint, file=sys.stderr)
    return 0


def cmd_unwrap_recovery(args):
    wrapped = read_key(args.key_recovery)
    try:
        key = load_private(args.recovery_key).decrypt(wrapped, OAEP)
    except ValueError as exc:
        # OAEP failure. Say what it means rather than leaking an oracle detail.
        raise SystemExit("the recovery key did not unwrap this data key "
                         "(wrong key, or the wrapper was altered): %s" % exc)
    if len(key) != 32:
        raise SystemExit("unwrapped data key is %d bytes, expected 32" % len(key))
    write_file_0600(args.out, key)
    if args.verbose:
        print("data_key_unwrapped=32", file=sys.stderr)
    return 0


def _open_v2(src, key):
    """Validate the v2 header and return (header_bytes, header_dict, aesgcm)."""
    magic = src.read(len(MAGIC))
    if magic != MAGIC:
        raise SystemExit("not a v2 envelope (bad magic %r)" % magic)
    hlen = int.from_bytes(read_exactly(src, 4), "big")
    if hlen <= 0 or hlen > 65536:
        raise SystemExit("implausible header length %d" % hlen)
    header = read_exactly(src, hlen)
    try:
        info = json.loads(header.decode("ascii"))
    except Exception as exc:
        raise SystemExit("unreadable envelope header: %s" % exc)
    if info.get("v") != 2 or info.get("cipher") != "AES-256-GCM":
        raise SystemExit("unsupported envelope version/cipher: %r" % info)
    try:
        nonce_prefix = bytes.fromhex(info["nonce_prefix"])
    except Exception:
        raise SystemExit("envelope header has no usable nonce_prefix")
    if len(nonce_prefix) != NONCE_PREFIX_LEN:
        raise SystemExit("nonce_prefix is %d bytes, expected %d"
                         % (len(nonce_prefix), NONCE_PREFIX_LEN))
    return header, info, AESGCM(key), nonce_prefix


def _walk_v2(src, header, aesgcm, nonce_prefix, sink):
    """Authenticate and decrypt every segment, passing plaintext to `sink`.

    `sink` is called only with bytes whose GCM tag has already verified, so no
    unauthenticated plaintext is ever produced or written. Returns
    (segments, plaintext_bytes).
    """
    prev_tag = b"\x00" * TAG_LEN
    segments = 0
    total = 0
    pending_len = None

    while True:
        if pending_len is None:
            lb = src.read(4)
            if len(lb) == 0:
                raise SystemExit("envelope ends before its final segment: truncated")
            if len(lb) != 4:
                raise SystemExit("truncated envelope: partial segment length")
            pending_len = int.from_bytes(lb, "big")

        if pending_len < TAG_LEN or pending_len > CHUNK + TAG_LEN:
            raise SystemExit("implausible segment length %d" % pending_len)

        ct = read_exactly(src, pending_len)
        lb_next = src.read(4)
        is_last = (len(lb_next) == 0)
        if is_last:
            pending_len = None
        else:
            if len(lb_next) != 4:
                raise SystemExit("truncated envelope: partial segment length")
            pending_len = int.from_bytes(lb_next, "big")

        aad = header + prev_tag + (b"\x01" if is_last else b"\x00")
        if segments >= MAX_CHUNKS:
            raise SystemExit("segment count exceeds the nonce counter space")
        nonce = nonce_prefix + segments.to_bytes(4, "big")
        try:
            pt = aesgcm.decrypt(nonce, ct, aad)
        except InvalidTag:
            raise SystemExit(
                "AUTHENTICATION FAILED at segment %d: the payload, its header or "
                "its key wrapper has been altered. Nothing from this segment was "
                "written." % segments)
        sink(pt)
        prev_tag = ct[-TAG_LEN:]
        total += len(pt)
        segments += 1
        if is_last:
            break

    extra = src.read(1)
    if extra:
        raise SystemExit("trailing data after the final segment: the envelope is malformed")
    return segments, total


def _unseal_v2(args, key):
    # Atomic publish. Each segment's tag is verified before its plaintext is
    # used, but a *later* segment can still fail -- and a truncated tail would
    # then leave a file that looks like a whole dump while being a prefix of
    # one. A backup that is silently short is more dangerous than no backup, so
    # the plaintext goes to a temporary path and is renamed into place only
    # after every segment has verified and the length matches the header.
    # On any failure the temporary file is removed: no partial plaintext is
    # ever published.
    tmp = args.out + ".partial"
    if os.path.exists(tmp):
        os.remove(tmp)
    try:
        with open(args.input, "rb") as src:
            header, info, aesgcm, nonce_prefix = _open_v2(src, key)
            with open(tmp, "wb") as dst:
                try:
                    segments, total = _walk_v2(src, header, aesgcm, nonce_prefix, dst.write)
                finally:
                    dst.flush()
                    os.fsync(dst.fileno())
        expected = info.get("plaintext_bytes")
        if isinstance(expected, int) and expected != total:
            # The header is authenticated, so a mismatch here means the writer
            # and the reader disagree -- a bug worth surfacing, not rounding away.
            raise SystemExit("plaintext length %d does not match the header's %d"
                             % (total, expected))
    except BaseException:
        # Covers SystemExit from the walker and any I/O error. Nothing partial
        # is left behind to be mistaken for a good dump.
        try:
            os.remove(tmp)
        except OSError:
            pass
        raise

    if os.path.exists(args.out):
        os.remove(args.out)
    os.replace(tmp, args.out)
    if args.verbose:
        print("plaintext_bytes=%d segments=%d" % (total, segments), file=sys.stderr)
    return 0


def _verify_v2(args, key):
    with open(args.input, "rb") as src:
        header, info, aesgcm, nonce_prefix = _open_v2(src, key)
        segments, total = _walk_v2(src, header, aesgcm, nonce_prefix, lambda _pt: None)
    print("verified segments=%d plaintext_bytes=%d fingerprint=%s"
          % (segments, total, info.get("recovery_fingerprint", "?")))
    return 0


def _unseal_v1(args, key):
    """Archival recovery for a pre-v2 artefact: AES-256-CBC with a PKCS7 pad.

    Kept so an existing backup can still be read. It cannot authenticate, which
    is exactly why nothing new is written in this format, and why the caller has
    to ask for it explicitly -- `--legacy` is a statement that the operator
    knows this file predates the authenticated format.
    """
    if not args.legacy:
        raise SystemExit(
            "this looks like a v1 (AES-CBC) artefact, which cannot authenticate "
            "itself. Re-run with --legacy to recover it as archival data.")
    if not args.iv:
        raise SystemExit("a v1 artefact needs --iv")
    iv = read_key(args.iv)
    if len(iv) != 16:
        raise SystemExit("iv must be 16 bytes, got %d" % len(iv))

    dec = Cipher(algorithms.AES(key), modes.CBC(iv)).decryptor()
    unpadder = PKCS7(128).unpadder()
    total = 0
    with open(args.input, "rb") as src, open(args.out, "wb") as dst:
        while True:
            block = src.read(CHUNK)
            if not block:
                break
            dst.write(unpadder.update(dec.update(block)))
            total += len(block)
        dst.write(unpadder.update(dec.finalize()))
        dst.write(dec.finalize())
    if args.verbose:
        print("plaintext_bytes=%d (v1, UNAUTHENTICATED)" % total, file=sys.stderr)
    return 0


def cmd_unseal(args):
    key = read_key(args.key_file)
    if len(key) != 32:
        raise SystemExit("data key must be 32 bytes, got %d" % len(key))

    with open(args.input, "rb") as probe:
        head = probe.read(len(MAGIC))
    if head == MAGIC:
        return _unseal_v2(args, key)
    return _unseal_v1(args, key)


def cmd_verify(args):
    key = read_key(args.key_file)
    if len(key) != 32:
        raise SystemExit("data key must be 32 bytes, got %d" % len(key))
    with open(args.input, "rb") as probe:
        head = probe.read(len(MAGIC))
    if head != MAGIC:
        raise SystemExit("verify requires a v2 envelope; this file is v1 and "
                         "cannot be authenticated")
    return _verify_v2(args, key)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("fingerprint", help="sha256 of a public key's SPKI")
    p.add_argument("--public", required=True)
    p.set_defaults(func=cmd_fingerprint)

    p = sub.add_parser("seal", help="AES-256-GCM the payload, wrap the data key")
    p.add_argument("--input", required=True)
    p.add_argument("--out-prefix", required=True)
    p.add_argument("--recovery-public", required=True)
    p.add_argument("--key-out",
                   help="write the raw data key to this file (owner-only) for a "
                        "DPAPI wrap; it is never printed to stdout")
    p.add_argument("--legacy", action="store_true",
                   help="accepted only to be refused; this tool never writes v1")
    p.add_argument("--verbose", action="store_true")
    p.set_defaults(func=cmd_seal)

    p = sub.add_parser("unwrap-recovery",
                       help="unwrap the data key with the recovery private key")
    p.add_argument("--key-recovery", required=True)
    p.add_argument("--recovery-key", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--verbose", action="store_true")
    p.set_defaults(func=cmd_unwrap_recovery)

    p = sub.add_parser("unseal", help="authenticate and decrypt the payload")
    p.add_argument("--input", required=True)
    p.add_argument("--key-file", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--iv", help="v1 artefacts only")
    p.add_argument("--legacy", action="store_true",
                   help="recover a v1 (unauthenticated) artefact")
    p.add_argument("--verbose", action="store_true")
    p.set_defaults(func=cmd_unseal)

    p = sub.add_parser("verify",
                       help="authenticate the payload without writing plaintext")
    p.add_argument("--input", required=True)
    p.add_argument("--key-file", required=True)
    p.set_defaults(func=cmd_verify)

    args = ap.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
