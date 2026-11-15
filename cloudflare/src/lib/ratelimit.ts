// Port of services.ClassifyRateLimit + parseFreeTierReset (backend/internal/services/rate_limit.go).

export type RateClass = 'free_tier' | 'transient'

export interface RateClassification {
  class: RateClass
  waitMs: number
}

const RESET_RE = /(?:comes back on|starts on)\s+(\d{1,2})\s+([A-Za-z]{3,9})\s+(\d{4})\s+at\s+(\d{1,2}):(\d{2})\s*UTC/i

const MONTHS: Record<string, number> = {
  jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6,
  jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12,
}

interface ErrorDetails {
  code: string
  type: string
  message: string
}

function errorDetails(text: string): ErrorDetails {
  try {
    const parsed = JSON.parse(text)
    const err = parsed?.error ?? {}
    return {
      code: String(err.code ?? ''),
      type: String(err.type ?? parsed?.type ?? ''),
      message: String(err.message ?? ''),
    }
  } catch {
    if (/free_tier_limit_reached/i.test(text)) {
      return { code: 'free_tier_limit_reached', type: '', message: text }
    }
    return { code: '', type: '', message: '' }
  }
}

export function classifyRateLimit(headers: Headers, bodyText: string): RateClassification {
  const { code, type, message } = errorDetails(bodyText)
  if (code === 'free_tier_limit_reached' || type === 'free_tier_limit_reached') {
    return { class: 'free_tier', waitMs: parseFreeTierReset(message) }
  }
  return { class: 'transient', waitMs: parseRetryAfter(headers, bodyText) }
}

export function parseFreeTierReset(message: string): number {
  const m = RESET_RE.exec(message)
  if (!m) return 0
  const month = MONTHS[m[2].slice(0, 3).toLowerCase()]
  if (!month) return 0
  const reset = Date.UTC(Number(m[3]), month - 1, Number(m[1]), Number(m[4]), Number(m[5]), 0)
  const wait = reset - Date.now()
  return wait > 0 ? wait + 60_000 : 0
}

export function parseRetryAfter(headers: Headers, bodyText: string): number {
  const raw = headers.get('retry-after')
  if (raw) {
    const secs = Number.parseInt(raw, 10)
    if (!Number.isNaN(secs) && secs > 0) return secs * 1000
    const when = Date.parse(raw)
    if (!Number.isNaN(when)) {
      const wait = when - Date.now()
      if (wait > 0) return wait
    }
  }
  // Gemini style: {"error":{"details":[{"retryDelay":"28s"}]}}
  try {
    const parsed = JSON.parse(bodyText)
    const delay =
      parsed?.error?.details?.find?.((d: { retryDelay?: string }) => d?.retryDelay)?.retryDelay ??
      parsed?.error?.retryDelay ??
      parsed?.retryDelay
    if (typeof delay === 'string') {
      const m = /^([\d.]+)s$/.exec(delay.trim())
      if (m) return Math.round(Number.parseFloat(m[1]) * 1000)
    }
  } catch {
    /* not JSON */
  }
  return 0
}
