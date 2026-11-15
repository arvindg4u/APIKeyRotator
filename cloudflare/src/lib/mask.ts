// Mask an upstream key the same way the Go backend logs do (utils.MaskAPIKeyDefault):
// keep a short prefix and suffix, never the whole value.
export function maskKey(key: string): string {
  if (!key) return ''
  if (key.length <= 12) return `${key.slice(0, 2)}****`
  return `${key.slice(0, 7)}*****${key.slice(-3)}`
}
