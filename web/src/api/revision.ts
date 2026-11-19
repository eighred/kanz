// Identity's HTTP revision and epoch domain is nonnegative int64. Keep the
// decimal text through JSON.parse and the reviewed mutation; never Number it.
export function isRevision(value: unknown): value is string {
  return typeof value === 'string' && /^(?:0|[1-9][0-9]{0,18})$/.test(value) && BigInt(value) <= 9223372036854775807n
}

export function requireRevision(value: unknown): string {
  if (!isRevision(value)) throw new Error('Identity returned an invalid revision; reload before editing.')
  return value
}
