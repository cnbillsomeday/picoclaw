#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""a2a sign.py — Ed25519 message signing for an A2A platform (stdlib only).

payload = "v1|{from}|{to}|{timestamp}|{nonce}|{sha256hex(content)}"
sig     = hex(Ed25519(seed, payload))

Usage:
  sign.py redeem --token <token> [--api URL]     # one-time: fetch your seed -> $A2A_KEYDIR/ed25519.seed (default ~/.a2a)
  sign.py sign --keyfile F --from SLUG --to SLUG --content TEXT
  sign.py pubkey --keyfile F                     # print base64url public key
  sign.py register --keyfile F --token <token>   # publish a self-held public key
  sign.py clock [--api URL]                      # clock skew self-check
  sign.py verify --pubkey B64 --from .. --to .. --timestamp .. --nonce .. --content .. --sig ..
"""
import argparse, base64, binascii, hashlib, json, os, sys, time, urllib.request

DEFAULT_API = os.environ.get("A2A_API", "http://127.0.0.1:21220")
UA = "a2a-sign/1.0 (+a2a)"
KEYDIR = os.environ.get("A2A_KEYDIR") or os.path.expanduser("~/.a2a")
SEEDFILE = os.path.join(KEYDIR, "ed25519.seed")

# ---------------- Ed25519 (RFC 8032 reference, pure python) ----------------
q = 2 ** 255 - 19
l = 2 ** 252 + 27742317777372353535851937790883648493


def sha512(s):
    return hashlib.sha512(s).digest()


def inv(x):
    return pow(x, q - 2, q)


d = -121665 * inv(121666) % q
I = pow(2, (q - 1) // 4, q)


def xrecover(y):
    xx = (y * y - 1) * inv(d * y * y + 1)
    x = pow(xx, (q + 3) // 8, q)
    if (x * x - xx) % q != 0:
        x = (x * I) % q
    if x % 2 != 0:
        x = q - x
    return x


By = 4 * inv(5) % q
Bx = xrecover(By)
B = (Bx % q, By % q)


def edwards(P, Q):
    x1, y1 = P
    x2, y2 = Q
    k = d * x1 * x2 * y1 * y2
    x3 = (x1 * y2 + x2 * y1) * inv(1 + k)
    y3 = (y1 * y2 + x1 * x2) * inv(1 - k)
    return (x3 % q, y3 % q)


def scalarmult(P, e):
    Q = (0, 1)
    while e:
        if e & 1:
            Q = edwards(Q, P)
        P = edwards(P, P)
        e >>= 1
    return Q


def encodepoint(P):
    x, y = P
    bits = [(y >> i) & 1 for i in range(255)] + [x & 1]
    return bytes(sum(bits[i * 8 + j] << j for j in range(8)) for i in range(32))


def publickey(seed):
    h = sha512(seed)
    a = 2 ** 254 + sum(2 ** i * ((h[i // 8] >> (i % 8)) & 1) for i in range(3, 254))
    return encodepoint(scalarmult(B, a))


def sign(seed, msg):
    h = sha512(seed)
    a = 2 ** 254 + sum(2 ** i * ((h[i // 8] >> (i % 8)) & 1) for i in range(3, 254))
    A = encodepoint(scalarmult(B, a))
    r = int.from_bytes(sha512(h[32:64] + msg), "little") % l
    R = encodepoint(scalarmult(B, r))
    S = (r + int.from_bytes(sha512(R + A + msg), "little") * a) % l
    return (R + S.to_bytes(32, "little")).hex()


def verify(pub, msg, sighex):
    try:
        sig = bytes.fromhex(sighex)
    except ValueError:
        return False
    if len(sig) != 64 or len(pub) != 32:
        return False
    R = sig[:32]
    S = int.from_bytes(sig[32:], "little")
    if S >= l:
        return False
    A = decodepoint(pub)
    if A is None:
        return False
    h = int.from_bytes(sha512(R + pub + msg), "little") % l
    return scalarmult(B, S) == edwards(decodepoint(R), scalarmult(A, h))


def decodepoint(s):
    if len(s) != 32:
        return None
    y = int.from_bytes(s, "little") & ((1 << 255) - 1)
    x = xrecover(y)
    if (x & 1) != (s[31] >> 7):
        x = q - x
    if x == 0 and (s[31] >> 7):
        return None
    return (x, y)


# ---------------- helpers ----------------
def payload(frm, to, ts, nonce, content):
    return "v1|%s|%s|%d|%s|%s" % (frm, to, ts, nonce, hashlib.sha256(content.encode()).hexdigest())


def b64url_decode(s):
    s = s.strip()
    pad = s + "=" * (-len(s) % 4)
    try:
        return base64.urlsafe_b64decode(pad)
    except Exception:
        return binascii.a2b_base64(s + "=" * (-len(s) % 4))


def b64url_encode(b):
    return base64.urlsafe_b64encode(b).decode().rstrip("=")


def load_seed(path):
    if not os.path.exists(path):
        sys.exit("no keyfile at %s — run: sign.py redeem --token <token>" % path)
    raw = open(path, "rb").read()
    if len(raw) == 32:          # 裸 seed（redeem 写的就是这个）
        return raw
    text = raw.decode("utf-8", "ignore").strip()
    for dec in (b64url_decode, bytes.fromhex):
        try:
            b = dec(text)
            if len(b) == 32:
                return b
        except Exception:
            pass
    sys.exit("keyfile %s: cannot decode 32-byte seed" % path)


def http_json(url, token=None, body=None, timeout=10):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method="POST" if data else "GET")
    req.add_header("User-Agent", UA)
    if data:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode() or "{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read().decode() or "{}")
        except Exception:
            return e.code, {}


def cmd_redeem(a):
    api = (a.api or DEFAULT_API).rstrip("/")
    st, body = http_json(api + "/a2a/keys/redeem", token=a.token, body={})
    if st != 200 or not body.get("seed_base64url"):
        sys.exit("redeem failed: http=%s %s" % (st, json.dumps(body)[:300]))
    seed = b64url_decode(body["seed_base64url"])
    if len(seed) != 32:
        sys.exit("redeem: platform returned a %d-byte seed (expected 32)" % len(seed))
    os.makedirs(KEYDIR, mode=0o700, exist_ok=True)
    tmp = a.keyfile + ".tmp"
    with open(tmp, "wb") as f:
        f.write(seed)
    os.chmod(tmp, 0o600)
    os.replace(tmp, a.keyfile)
    print("REDEEM OK slug=%s first=%s pubkey=%s keyfile=%s (600)"
          % (body.get("slug"), body.get("first_redeem"), b64url_encode(publickey(seed)), a.keyfile))


def cmd_sign(a):
    seed = load_seed(a.keyfile)
    ts = int(time.time())
    nonce = os.urandom(16).hex()
    p = payload(a.frm, a.to, ts, nonce, a.content)
    sig = sign(seed, p.encode())
    print(json.dumps({"content": a.content, "timestamp": ts, "nonce": nonce, "sig": sig}, ensure_ascii=False))


def cmd_pubkey(a):
    seed = load_seed(a.keyfile)
    print(b64url_encode(publickey(seed)))


def cmd_register(a):
    seed = load_seed(a.keyfile)
    pub = b64url_encode(publickey(seed))
    api = (a.api or DEFAULT_API).rstrip("/")
    st, body = http_json(api + "/a2a/keys/register", token=a.token, body={"public_key": pub})
    print("REGISTER http=%s %s" % (st, json.dumps(body, ensure_ascii=False)[:300]))


def cmd_clock(a):
    api = (a.api or DEFAULT_API).rstrip("/")
    offs = []
    for _ in range(3):
        t0 = time.time()
        req = urllib.request.Request(api + "/healthz")
        req.add_header("User-Agent", UA)
        with urllib.request.urlopen(req, timeout=8) as r:
            srv = r.headers.get("Date")
            t1 = time.time()
        if not srv:
            continue
        import email.utils
        offs.append((email.utils.mktime_tz(email.utils.parsedate_tz(srv)) + (t1 - t0) / 2) - time.time())
    if not offs:
        sys.exit("clock: cannot read server Date header")
    offs.sort()
    off = offs[len(offs) // 2]
    print("CLOCK OK skew=%.1fs" % off if abs(off) < 60 else "CLOCK DRIFT %.1fs" % off)
    sys.exit(0 if abs(off) < 300 else 2)


def cmd_verify(a):
    pub = b64url_decode(a.pubkey)
    if len(pub) != 32:
        sys.exit("verify: public key must be 32 bytes")
    p = payload(a.frm, a.to, a.timestamp, a.nonce, a.content)
    print("VERIFY OK" if verify(pub, p.encode(), a.sig) else "VERIFY FAIL")


def main():
    ap = argparse.ArgumentParser(prog="sign.py")
    sub = ap.add_subparsers(dest="cmd", required=True)

    r = sub.add_parser("redeem"); r.add_argument("--token", required=True)
    r.add_argument("--api"); r.add_argument("--keyfile", default=SEEDFILE); r.set_defaults(f=cmd_redeem)

    s = sub.add_parser("sign")
    s.add_argument("--keyfile", default=SEEDFILE); s.add_argument("--from", dest="frm", required=True)
    s.add_argument("--to", required=True); s.add_argument("--content", required=True); s.set_defaults(f=cmd_sign)

    p = sub.add_parser("pubkey"); p.add_argument("--keyfile", default=SEEDFILE); p.set_defaults(f=cmd_pubkey)

    g = sub.add_parser("register"); g.add_argument("--token", required=True)
    g.add_argument("--api"); g.add_argument("--keyfile", default=SEEDFILE); g.set_defaults(f=cmd_register)

    c = sub.add_parser("clock"); c.add_argument("--api"); c.set_defaults(f=cmd_clock)

    v = sub.add_parser("verify")
    for k in ("--pubkey", "--from", "--to", "--nonce", "--content", "--sig"):
        v.add_argument(k, dest=k.strip("-").replace("from", "frm"), required=True)
    v.add_argument("--timestamp", type=int, required=True)
    v.set_defaults(f=cmd_verify)

    a = ap.parse_args()
    a.f(a)


if __name__ == "__main__":
    main()
