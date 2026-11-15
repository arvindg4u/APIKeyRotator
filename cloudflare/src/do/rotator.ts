import { DurableObject } from 'cloudflare:workers'
import type { RotatorSnapshot, StoredEvent, StoredLog } from '../lib/types'

// One Rotator per proxy config slug. Owns the round-robin counter, per-key
// cooldowns and per-config telemetry. SQLite-backed storage (declared in
// wrangler.jsonc `exports`), which is what the Workers Free plan supports.

const MAX_EVENTS = 200
const MAX_LOGS = 200
const MAX_RPM = 60

interface Cooldown {
  until: number
  className: string
}

interface Stats {
  total: number
  statuses: Record<string, number>
  rpm: number[]
  rpmMinute: number
}

interface State {
  counter: number
  cooldowns: Record<string, Cooldown>
  stats: Stats
  eventCount: number
  logCount: number
}

const KEY_STATE = 'state'
const KEY_EVENTS = 'events'
const KEY_LOGS = 'logs'

function emptyState(): State {
  return {
    counter: 0,
    cooldowns: {},
    stats: { total: 0, statuses: {}, rpm: new Array(MAX_RPM).fill(0), rpmMinute: 0 },
    eventCount: 0,
    logCount: 0,
  }
}

export class Rotator extends DurableObject {
  // Round-robin over the healthy keys; fall back to the soonest-recovering
  // cooling key; null when every key was already tried in this request.
  async next(activeIds: number[], triedIds: number[]): Promise<{ id: number; waitMs: number } | null> {
    const state = await this.state()
    const now = Date.now()
    const tried = new Set(triedIds)
    const healthy: number[] = []
    const cooling: Array<{ id: number; waitMs: number }> = []

    for (const id of activeIds) {
      if (tried.has(id)) continue
      const cd = state.cooldowns[String(id)]
      if (cd && cd.until > now) {
        cooling.push({ id, waitMs: cd.until - now })
      } else {
        if (cd) delete state.cooldowns[String(id)]
        healthy.push(id)
      }
    }

    let chosen: number | null = null
    if (healthy.length > 0) {
      state.counter += 1
      chosen = healthy[(state.counter - 1) % healthy.length]
    } else if (cooling.length > 0) {
      cooling.sort((a, b) => a.waitMs - b.waitMs)
      chosen = cooling[0].id
    }

    await this.putState(state)
    if (chosen === null) return null
    const cd = state.cooldowns[String(chosen)]
    return { id: chosen, waitMs: cd && cd.until > now ? cd.until - now : 0 }
  }

  async markCooldown(id: number, className: string, waitMs: number): Promise<void> {
    const state = await this.state()
    state.cooldowns[String(id)] = { until: Date.now() + Math.max(0, waitMs), className }
    await this.putState(state)
  }

  async recordRequest(slug: string, status: number, latencyMs: number): Promise<void> {
    const state = await this.state()
    state.stats.total += 1
    state.stats.statuses[String(status)] = (state.stats.statuses[String(status)] ?? 0) + 1
    this.bumpMinute(state)
    await this.putState(state)

    await this.appendEvent({
      level: status >= 500 ? 'error' : status >= 400 ? 'warning' : 'info',
      kind: 'request',
      slug,
      status,
      latencyMs,
      message: `${slug} -> ${status} (${latencyMs}ms)`,
    })
  }

  async recordCooldown(slug: string, keyMasked: string, className: string, waitMs: number): Promise<void> {
    const seconds = Math.round(waitMs / 1000)
    await this.appendEvent({
      level: 'warning',
      kind: 'cooldown',
      slug,
      keyMasked,
      class: className,
      message: `key ${keyMasked} cooling ${seconds}s (${className})`,
    })
  }

  async snapshot(): Promise<RotatorSnapshot> {
    const state = await this.state()
    const now = Date.now()
    let cooling = 0
    for (const cd of Object.values(state.cooldowns)) if (cd.until > now) cooling += 1
    return { total: state.stats.total, statuses: { ...state.stats.statuses }, rpm: [...state.stats.rpm], cooling }
  }

  async eventsSince(sinceId: number, limit: number): Promise<StoredEvent[]> {
    const events = (await this.ctx.storage.get<StoredEvent[]>(KEY_EVENTS)) ?? []
    return events.filter((e) => e.id > sinceId).slice(-limit)
  }

  async logsSince(sinceId: number, limit: number): Promise<StoredLog[]> {
    const logs = (await this.ctx.storage.get<StoredLog[]>(KEY_LOGS)) ?? []
    return logs.filter((l) => l.id > sinceId).slice(-limit)
  }

  private async state(): Promise<State> {
    return (await this.ctx.storage.get<State>(KEY_STATE)) ?? emptyState()
  }

  private async putState(state: State): Promise<void> {
    await this.ctx.storage.put(KEY_STATE, state)
  }

  private bumpMinute(state: State): void {
    const minute = Math.floor(Date.now() / 60000) * 60000
    const rpm = state.stats.rpm
    if (!state.stats.rpmMinute) state.stats.rpmMinute = minute
    const gap = Math.round((minute - state.stats.rpmMinute) / 60000)
    if (gap <= 0) {
      rpm[rpm.length - 1] += 1
      return
    }
    if (gap >= rpm.length) {
      rpm.fill(0)
    } else {
      for (let i = 0; i < gap; i++) {
        rpm.shift()
        rpm.push(0)
      }
    }
    state.stats.rpmMinute = minute
    rpm[rpm.length - 1] += 1
  }

  private async appendEvent(partial: Omit<StoredEvent, 'id' | 'time'>): Promise<void> {
    const state = await this.state()
    state.eventCount += 1
    await this.putState(state)

    const events = (await this.ctx.storage.get<StoredEvent[]>(KEY_EVENTS)) ?? []
    const event: StoredEvent = { id: state.eventCount, time: Date.now(), ...partial }
    events.push(event)
    while (events.length > MAX_EVENTS) events.shift()
    await this.ctx.storage.put(KEY_EVENTS, events)

    await this.appendLog({ level: event.level, message: event.message ?? '' })
  }

  private async appendLog(partial: Omit<StoredLog, 'id' | 'time'>): Promise<void> {
    const state = await this.state()
    state.logCount += 1
    await this.putState(state)

    const logs = (await this.ctx.storage.get<StoredLog[]>(KEY_LOGS)) ?? []
    const log: StoredLog = { id: state.logCount, time: Date.now(), ...partial }
    logs.push(log)
    while (logs.length > MAX_LOGS) logs.shift()
    await this.ctx.storage.put(KEY_LOGS, logs)
  }
}
