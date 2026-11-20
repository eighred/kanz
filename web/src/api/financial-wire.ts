// Validate the JSON representation before passing it to the exact renderer.
// Missing optional message fields are unavailable. Omitted protobuf scalars
// inside a present message use their defaults; an explicit null is not a scalar.
function object(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

export function checkWireDecimal(value: unknown): void {
  if (value == null) return
  if (!object(value) || 'amount' in value || 'currency_code' in value ||
      (value.coefficient !== undefined && typeof value.coefficient !== 'string') ||
      (value.exponent !== undefined && !Number.isInteger(value.exponent))) {
    throw new Error('The service returned an invalid decimal representation.')
  }
}

export function checkWireMoney(value: unknown): void {
  if (value == null) return
  if (!object(value) || (value.currency_code !== undefined && typeof value.currency_code !== 'string')) {
    throw new Error('The service returned an invalid money representation.')
  }
  checkWireDecimal(value.amount)
}
