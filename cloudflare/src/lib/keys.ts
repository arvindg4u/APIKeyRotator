import type { ProxyConfig } from './types'

// Port of services.ResolveUpstreamKeyTarget / InjectUpstreamKey.
// openai_compatible -> Authorization: Bearer <key>; gemini_native -> x-goog-api-key
// (or ?key= when the config picks the query location); anthropic_native -> x-api-key.
// Per-config api_key_name / api_key_location always win.

export function apiFormatOf(config: ProxyConfig): string {
  return (config.api_format || 'openai_compatible').trim()
}

export function resolveKeyTarget(config: ProxyConfig, apiFormat: string): { name: string; location: string } {
  let name = ''
  let location = ''
  switch (apiFormat) {
    case 'gemini_native':
      name = 'x-goog-api-key'
      location = 'header'
      break
    case 'anthropic_native':
      name = 'x-api-key'
      location = 'header'
      break
    case 'openai_compatible':
      name = 'Authorization'
      location = 'header'
      break
  }

  const configuredName = config.api_key_name?.trim()
  const configuredLocation = config.api_key_location?.trim().toLowerCase()
  if (configuredName) name = configuredName
  if (configuredLocation) location = configuredLocation

  if (apiFormat === 'gemini_native' && location === 'query' && !configuredName) name = 'key'

  return { name, location }
}

export function injectKey(headers: Headers, params: URLSearchParams, config: ProxyConfig, apiFormat: string, key: string): boolean {
  const { name, location } = resolveKeyTarget(config, apiFormat)
  if (!name || !location) return false
  if (location === 'header') {
    headers.set(name, apiFormat === 'openai_compatible' ? `Bearer ${key}` : key)
    return true
  }
  if (location === 'query') {
    params.set(name, key)
    return true
  }
  return false
}
