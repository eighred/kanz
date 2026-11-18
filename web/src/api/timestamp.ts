export function isTimestamp(value: unknown): value is string {
  if (typeof value !== 'string' || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d{1,9})?(?:Z|[+-]\d\d:\d\d)$/.test(value) ||
      !Number.isFinite(Date.parse(value))) return false
  // Date.parse normalizes impossible calendar dates, including February 30.
  const wallTime = value.slice(0, 19)
  return new Date(wallTime + 'Z').toISOString().startsWith(wallTime)
}
