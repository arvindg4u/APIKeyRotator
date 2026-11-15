// Admin session token: HS256 JWT built with Web Crypto only (no dependencies).
// Mirrors backend/internal/auth/token.go: claims {sub:"admin", username, iat, exp}.

const enc = new TextEncoder()

function b64url(bytes: Uint8Array | ArrayBuffer): string {
  const b = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes)
  let s = ''
  for (let i = 0; i < b.length; i++) s += String.fromCharCode(b[i])
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function b64urlDecode(value: string): Uint8Array {
  let s = value.replace(/-/g, '+').replace(/_/g, '/')
  const pad = s.length % 4 ? '='.repeat(4 - (s.length % 4)) : ''
  const bin = atob(s + pad)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

async function hmacKey(secret: string): Promise<CryptoKey> {
  return crypto.subtle.importKey('raw', enc.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign', 'verify'])
}

export async function signToken(secret: string, username: string, ttlSeconds = 86400): Promise<string> {
  const now = Math.floor(Date.now() / 1000)
  const header = b64url(enc.encode(JSON.stringify({ alg: 'HS256', typ: 'JWT' })))
  const payload = b64url(enc.encode(JSON.stringify({ sub: 'admin', username, iat: now, exp: now + ttlSeconds })))
  const data = `${header}.${payload}`
  const sig = await crypto.subtle.sign('HMAC', await hmacKey(secret), enc.encode(data))
  return `${data}.${b64url(sig)}`
}

export async function verifyToken(secret: string, token: string): Promise<string | null> {
  const parts = token.trim().split('.')
  if (parts.length !== 3) return null
  const data = `${parts[0]}.${parts[1]}`
  try {
    const ok = await crypto.subtle.verify('HMAC', await hmacKey(secret), b64urlDecode(parts[2]), enc.encode(data))
    if (!ok) return null
    const payload = JSON.parse(new TextDecoder().decode(b64urlDecode(parts[1])))
    if (payload.exp && Math.floor(Date.now() / 1000) >= payload.exp) return null
    return typeof payload.username === 'string' ? payload.username : ''
  } catch {
    return null
  }
}

export function constantTimeEquals(a: string, b: string): boolean {
  const ab = enc.encode(a)
  const bb = enc.encode(b)
  if (ab.length !== bb.length) return false
  let diff = 0
  for (let i = 0; i < ab.length; i++) diff |= ab[i] ^ bb[i]
  return diff === 0
}
