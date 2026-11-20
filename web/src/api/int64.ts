// ProtoJSON int64 values retain all digits as decimal strings.
export function isNonnegativeInt64(value: unknown): value is string {
  return typeof value === 'string' && /^(?:0|[1-9][0-9]{0,18})$/.test(value) && BigInt(value) <= 9223372036854775807n
}
