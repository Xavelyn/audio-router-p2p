/**
 * Computes an 8-character lowercase hexadecimal hash identical to Go's urn.Checksum(val).
 * Uses SHA-256 and extracts the first 4 bytes (8 hex characters).
 */
export function urnChecksum(ascii: string): string {
  if (!ascii) return '';

  function rightRotate(value: number, amount: number): number {
    return (value >>> amount) | (value << (32 - amount));
  }

  const words: number[] = [];
  const asciiBitLength = ascii.length * 8;
  const hash = [
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
    0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ];
  const k = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
  ];

  for (let i = 0; i < ascii.length; i++) {
    words[i >> 2] |= (ascii.charCodeAt(i) & 0xff) << ((3 - (i % 4)) * 8);
  }
  words[ascii.length >> 2] |= 0x80 << ((3 - (ascii.length % 4)) * 8);

  const totalWords = (((ascii.length + 8) >> 6) + 1) * 16;
  while (words.length < totalWords - 2) {
    words.push(0);
  }
  words[totalWords - 2] = Math.floor(asciiBitLength / 0x100000000);
  words[totalWords - 1] = asciiBitLength & 0xffffffff;

  for (let b = 0; b < words.length; b += 16) {
    const w = words.slice(b, b + 16);
    let a = hash[0], c = hash[1], d = hash[2], e = hash[3];
    let f = hash[4], g = hash[5], h = hash[6], l = hash[7];

    for (let i = 0; i < 64; i++) {
      if (i >= 16) {
        const s0 = rightRotate(w[i - 15], 7) ^ rightRotate(w[i - 15], 18) ^ (w[i - 15] >>> 3);
        const s1 = rightRotate(w[i - 2], 17) ^ rightRotate(w[i - 2], 19) ^ (w[i - 2] >>> 10);
        w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
      }
      const ch = (f & g) ^ (~f & h);
      const temp1 = (l + (rightRotate(f, 6) ^ rightRotate(f, 11) ^ rightRotate(f, 25)) + ch + k[i] + w[i]) | 0;
      const maj = (a & c) ^ (a & d) ^ (c & d);
      const temp2 = ((rightRotate(a, 2) ^ rightRotate(a, 13) ^ rightRotate(a, 22)) + maj) | 0;

      l = h;
      h = g;
      g = f;
      f = (e + temp1) | 0;
      e = d;
      d = c;
      c = a;
      a = (temp1 + temp2) | 0;
    }
    hash[0] = (hash[0] + a) | 0;
    hash[1] = (hash[1] + c) | 0;
    hash[2] = (hash[2] + d) | 0;
    hash[3] = (hash[3] + e) | 0;
    hash[4] = (hash[4] + f) | 0;
    hash[5] = (hash[5] + g) | 0;
    hash[6] = (hash[6] + h) | 0;
    hash[7] = (hash[7] + l) | 0;
  }

  return (hash[0] >>> 0).toString(16).padStart(8, '0');
}
