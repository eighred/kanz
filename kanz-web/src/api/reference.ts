import { api, ApiError } from './client'

export interface SecurityRecord {
  instrument_id: string
  asset_class: string
  currency_code: string
  description: string
  issuer_id: string
  sector: { taxonomy: string; code: string; name: string }
  as_of: string
  identifiers: { isin: string; cusip: string; sedol: string; figi: string; ric: string }
  provenance: Record<string, string>
}

export interface DataException {
  ID: string; Kind: string; InstrumentID: string; Detail: string; Status: string
  DetectedAt: string; overrideCount: number
}

export interface PriceObservation {
  instrument_id: string; has_price: boolean; chosen?: string
  exceptions: number; stale_candidates: number; observed_at: string; observation_only: true
}

function parsePrice(value: unknown, id: string): PriceObservation {
  const invalid = () => { throw new Error('Invalid price observation.') }
  if (!value || typeof value !== 'object' || Array.isArray(value)) return invalid()
  const row = value as Record<string, unknown>
  if (row.instrument_id !== id || row.observation_only !== true || typeof row.has_price !== 'boolean' ||
      typeof row.observed_at !== 'string' || !Number.isFinite(Date.parse(row.observed_at)) ||
      !Number.isSafeInteger(row.exceptions) || (row.exceptions as number) < 0 ||
      !Number.isSafeInteger(row.stale_candidates) || (row.stale_candidates as number) < 0 ||
      (row.stale_candidates as number) > (row.exceptions as number)) return invalid()
  if (row.has_price ? (typeof row.chosen !== 'string' || row.chosen.length > 256 || !/^-?\d+(\.\d+)?$/.test(row.chosen)) : row.chosen !== undefined) return invalid()
  return { instrument_id: id, has_price: row.has_price, ...(row.has_price ? { chosen: row.chosen as string } : {}),
    exceptions: row.exceptions as number, stale_candidates: row.stale_candidates as number,
    observed_at: row.observed_at, observation_only: true }
}

function stringObject(value: unknown, keys: string[]): Record<string, string> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid reference-data response.')
  const row = value as Record<string, unknown>
  if (keys.some((key) => typeof row[key] !== 'string')) throw new Error('Invalid reference-data response.')
  return Object.fromEntries(keys.map((key) => [key, row[key] as string]))
}

function stringMap(value: unknown): Record<string, string> {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('Invalid reference-data response.')
  const entries = Object.entries(value)
  if (entries.some(([, item]) => typeof item !== 'string')) throw new Error('Invalid reference-data response.')
  return Object.fromEntries(entries.sort(([a], [b]) => a.localeCompare(b)))
}

function parseSecurity(value: unknown, requestedID: string): SecurityRecord {
  const row = stringObject(value, ['instrument_id', 'asset_class', 'currency_code', 'description', 'issuer_id', 'as_of']) as Record<string, unknown>
  if (row.instrument_id !== requestedID || (row.as_of !== '' && !Number.isFinite(Date.parse(row.as_of as string)))) throw new Error('Invalid reference-data response.')
  return {
    instrument_id: row.instrument_id as string, asset_class: row.asset_class as string,
    currency_code: row.currency_code as string, description: row.description as string,
    issuer_id: row.issuer_id as string, as_of: row.as_of as string,
    sector: stringObject((value as Record<string, unknown>).sector, ['taxonomy', 'code', 'name']) as SecurityRecord['sector'],
    identifiers: stringObject((value as Record<string, unknown>).identifiers, ['isin', 'cusip', 'sedol', 'figi', 'ric']) as SecurityRecord['identifiers'],
    provenance: stringMap((value as Record<string, unknown>).provenance),
  }
}

function parseException(value: unknown): DataException {
  const row = stringObject(value, ['ID', 'Kind', 'InstrumentID', 'Detail', 'Status', 'DetectedAt']) as Record<string, unknown>
  if (!row.ID || !row.Kind || !row.InstrumentID || !row.Status || !Number.isFinite(Date.parse(row.DetectedAt as string))) throw new Error('Invalid reference-data response.')
  const overrides = (value as Record<string, unknown>).Overrides
  if (overrides !== null && !Array.isArray(overrides)) throw new Error('Invalid reference-data response.')
  return { ...(row as unknown as Omit<DataException, 'overrideCount'>), overrideCount: overrides?.length ?? 0 }
}

export const reference = {
  price: (id: string) => api.get<unknown>(`/api/v1/prices/${encodeURIComponent(id)}`).then((row) => parsePrice(row, id)),
  security: (id: string) => api.get<unknown>(`/api/v1/securities/${encodeURIComponent(id)}`).then((row) => parseSecurity(row, id)),
  async exceptions(): Promise<DataException[]> {
    const body = await api.get<unknown>('/api/v1/exceptions')
    if (!Array.isArray(body)) throw new Error('Invalid reference-data response.')
    return body.map(parseException)
  },
}

export function describeReference(error: unknown): string {
  if (error instanceof ApiError && error.status === 403) return 'Your account is not permitted to read reference data.'
  if (error instanceof ApiError && error.status === 404) return 'No reference-data record was found for that identifier, or this surface is unavailable.'
  if (error instanceof ApiError && error.status === 503) return 'Reference data is currently unavailable.'
  return 'Reference data could not be verified.'
}
