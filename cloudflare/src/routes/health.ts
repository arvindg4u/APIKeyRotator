import { Hono } from 'hono'
import type { Env } from '../lib/types'

export const healthRoutes = new Hono<{ Bindings: Env }>()

healthRoutes.get('/', (c) => c.json({ status: 'ok' }))
