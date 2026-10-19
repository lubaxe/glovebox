'use strict';

// Makes an age key pair (https://age-encryption.org) in the browser, for
// recovering the encrypted drive. The page is served over plain HTTP, where
// browsers don't offer WebCrypto, so X25519 (RFC 7748) and bech32 are done
// here: the secret is 32 bytes from crypto.getRandomValues, the public key
// is X25519(secret, 9). Only the public key is sent to Glovebox.

(function (root) {
  const P = (1n << 255n) - 19n;
  const A24 = 121665n;
  const mod = (a) => { const r = a % P; return r >= 0n ? r : r + P; };
  function inv(a) { // a^(p-2) mod p
    let r = 1n, b = mod(a), e = P - 2n;
    while (e > 0n) { if (e & 1n) r = (r * b) % P; b = (b * b) % P; e >>= 1n; }
    return r;
  }

  // x25519 multiplies the point with u-coordinate u by the clamped scalar k.
  function x25519(k, u) {
    const kb = Uint8Array.from(k);
    kb[0] &= 248; kb[31] &= 127; kb[31] |= 64;
    let s = 0n;
    for (let i = 31; i >= 0; i--) s = (s << 8n) | BigInt(kb[i]);
    let x2 = 1n, z2 = 0n, x3 = u, z3 = 1n, swap = 0n;
    for (let t = 254n; t >= 0n; t--) {
      const bit = (s >> t) & 1n;
      if (swap ^ bit) { [x2, x3] = [x3, x2]; [z2, z3] = [z3, z2]; }
      swap = bit;
      const a = mod(x2 + z2), aa = (a * a) % P, b = mod(x2 - z2), bb = (b * b) % P, e = mod(aa - bb);
      const c = mod(x3 + z3), d = mod(x3 - z3), da = (d * a) % P, cb = (c * b) % P;
      x3 = mod((da + cb) ** 2n);
      z3 = mod(u * mod((da - cb) ** 2n));
      x2 = (aa * bb) % P;
      z2 = mod(e * (aa + A24 * e));
    }
    if (swap) { [x2, x3] = [x3, x2]; [z2, z3] = [z3, z2]; }
    let r = (x2 * inv(z2)) % P;
    const out = new Uint8Array(32);
    for (let i = 0; i < 32; i++) { out[i] = Number(r & 255n); r >>= 8n; }
    return out;
  }

  const CHARSET = 'qpzry9x8gf2tvdw0s3jn54khce6mua7l';
  function polymod(values) {
    const GEN = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3];
    let chk = 1;
    for (const v of values) {
      const top = chk >>> 25;
      chk = ((chk & 0x1ffffff) << 5) ^ v;
      for (let i = 0; i < 5; i++) if ((top >>> i) & 1) chk ^= GEN[i];
    }
    return chk;
  }
  function bech32(hrp, bytes) {
    const data = [];
    let acc = 0, bits = 0;
    for (const b of bytes) {
      acc = (acc << 8) | b; bits += 8;
      while (bits >= 5) { bits -= 5; data.push((acc >> bits) & 31); }
    }
    if (bits > 0) data.push((acc << (5 - bits)) & 31);
    const expand = [...hrp].map((c) => c.charCodeAt(0) >> 5).concat([0], [...hrp].map((c) => c.charCodeAt(0) & 31));
    const pm = polymod(expand.concat(data, [0, 0, 0, 0, 0, 0])) ^ 1;
    const check = [0, 1, 2, 3, 4, 5].map((i) => (pm >>> (5 * (5 - i))) & 31);
    return `${hrp}1${data.concat(check).map((d) => CHARSET[d]).join('')}`;
  }

  // fromSecret returns the key pair for a 32-byte secret.
  function fromSecret(secret) {
    return {
      publicKey: bech32('age', x25519(secret, 9n)),
      secretKey: bech32('age-secret-key-', secret).toUpperCase(),
    };
  }

  // generate makes a new key pair.
  function generate() {
    const secret = new Uint8Array(32);
    crypto.getRandomValues(secret);
    return fromSecret(secret);
  }

  // keyFile is the key pair as age-keygen writes it.
  function keyFile(pair, created = new Date()) {
    return `# created: ${created.toISOString().replace(/\.\d+Z$/, 'Z')}\n# public key: ${pair.publicKey}\n${pair.secretKey}\n`;
  }

  const api = { generate, fromSecret, keyFile };
  if (typeof module !== 'undefined') module.exports = api;
  else root.GloveboxAgeKey = api;
})(typeof window !== 'undefined' ? window : globalThis);
