import { isNonnegativeInt64 } from './int64'

// Identity's HTTP revision and epoch domain is nonnegative int64. Keep the
// decimal text through JSON.parse and the reviewed mutation; never Number it.
export const isRevision = isNonnegativeInt64

export function requireRevision(value: unknown): string {
  if (!isRevision(value)) throw new Error('Identity returned an invalid revision; reload before editing.')
  return value
}
