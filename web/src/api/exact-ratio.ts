function invalid(): never { throw new Error('Invalid exact financial amount.') }

// Financial strings never pass through Number, parseFloat, or Intl rounding.
// Ratios such as 1/3 retain their exact fraction instead of a made-up last digit.
export function exactRatio(value: unknown): { text: string; numerator: bigint; denominator: bigint } {
  if (typeof value !== 'string' || value.length > 400) return invalid()
  const s = value
  if (!/^-?[0-9]+(?:\.[0-9]+|\/[1-9][0-9]*)?$/.test(s)) return invalid()
  let numerator: bigint, denominator: bigint
  if (s.includes('/')) {
    const parts = s.split('/')
    numerator = BigInt(parts[0]!)
    denominator = BigInt(parts[1]!)
  } else {
    const parts = s.split('.')
    numerator = BigInt(parts.join(''))
    denominator = 10n ** BigInt(parts[1]?.length ?? 0)
  }
  let a = numerator < 0n ? -numerator : numerator, b = denominator
  while (b) { const remainder = a % b; a = b; b = remainder }
  numerator /= a; denominator /= a
  if ((numerator < 0n ? -numerator : numerator).toString(2).length > 512 || denominator.toString(2).length > 512) return invalid()
  return { text: s, numerator, denominator }
}
