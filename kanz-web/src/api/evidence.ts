import { api, ApiError } from './client'
export const reportTemplates = ['authz-decisions', 'command-outcomes', 'data-quality', 'full-log'] as const
export type ReportTemplate = typeof reportTemplates[number]
export interface EvidenceRecord { seq: string; event_id: string; occurred_at: string; event_type: string; kind: string; source: string; correlation_id: string; tenant_id: string }
export interface EvidencePage { version: 2; tenant_id: string; template: ReportTemplate; title: string; generated_at: string; integrity: { state: 'not_requested' }; count: number; after_cursor: string; complete: boolean; next_cursor: string; records: EvidenceRecord[] }
export interface ControlEvidence { id: string; category: string; count: number; minimum: number; samples: string[]; minimum_met: boolean }
export interface ControlEvidenceReport { version: 2; tenant_id: string; coverage: 'complete'; assessment: 'not_assessed'; from: string; to: string; total_count: number; controls: ControlEvidence[]; gaps: string[]; minimums_met: boolean }
const fail = (): never => { throw new Error('Evidence response could not be verified.') }
function object(value: unknown): Record<string, unknown> { if (!value || typeof value !== 'object' || Array.isArray(value)) return fail(); return value as Record<string, unknown> }
function text(value: unknown, empty = false): string { if (typeof value !== 'string' || value.length > 1024 || (!empty && !value.trim())) return fail(); return value }
function timestamp(value: unknown): string { const s = text(value); if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(s) || !Number.isFinite(Date.parse(s))) return fail(); return s }
function count(value: unknown): number { if (!Number.isSafeInteger(value) || (value as number) < 0 || (value as number) > 10000) return fail(); return value as number }
function sequence(value: unknown): string { const s = text(value); if (!/^[1-9][0-9]{0,18}$/.test(s) || BigInt(s) > 9223372036854775807n) return fail(); return s }
export function parseEvidencePage(value: unknown, tenant: string, template: ReportTemplate, after = ''): EvidencePage {
  const row = object(value)
  if (row.version !== 2 || row.tenant_id !== tenant || row.template !== template || object(row.integrity).state !== 'not_requested' || typeof row.complete !== 'boolean' || !Array.isArray(row.records) || row.records.length > 100 || row.count !== row.records.length) return fail()
  let prior = after ? BigInt(sequence(after)) : 0n
  const ids = new Set<string>()
  const records = row.records.map(value => {
    const r = object(value), seq = sequence(r.seq), id = text(r.event_id)
    if (r.tenant_id !== tenant || BigInt(seq) <= prior || ids.has(id)) return fail()
    prior = BigInt(seq); ids.add(id)
    return { seq, event_id: id, occurred_at: timestamp(r.occurred_at), event_type: text(r.event_type, true), kind: text(r.kind), source: text(r.source, true), correlation_id: text(r.correlation_id, true), tenant_id: tenant }
  })
  const next = text(row.next_cursor, true)
  if (row.complete ? next !== '' : records.length === 0 || next !== records.at(-1)?.seq) return fail()
  return { version: 2, tenant_id: text(tenant), template, title: text(row.title), generated_at: timestamp(row.generated_at), integrity: { state: 'not_requested' }, count: records.length, after_cursor: after, complete: row.complete, next_cursor: next, records }
}
export function parseControlEvidence(value: unknown, tenant: string, from: string, to: string): ControlEvidenceReport {
  const row = object(value)
  if (row.version !== 2 || row.tenant_id !== tenant || row.coverage !== 'complete' || row.assessment !== 'not_assessed' || typeof row.satisfied !== 'boolean' || !Array.isArray(row.controls) || row.controls.length !== 4) return fail()
  const start = timestamp(row.from), end = timestamp(row.to)
  if (Date.parse(start) !== Date.parse(from) || Date.parse(end) < Date.parse(start) || (to && Date.parse(end) !== Date.parse(to))) return fail()
  const ids = new Set<string>()
  const controls = row.controls.map(value => {
    const r = object(value), definition = object(r.control), id = text(definition.id), observed = count(r.count), minimum = count(definition.min_per_window)
    const samples = r.samples === null && observed === 0 ? [] : r.samples
    if (ids.has(id) || minimum === 0 || typeof r.satisfied !== 'boolean' || r.satisfied !== (observed >= minimum) || !Array.isArray(samples) || samples.length > 5 || samples.length > observed) return fail()
    ids.add(id)
    return { id, category: text(definition.category), count: observed, minimum, samples: samples.map(v => text(v)), minimum_met: r.satisfied }
  })
  if (['CC6.1', 'CC7.2', 'CC8.1', 'PI1.1'].some(id => !ids.has(id))) return fail()
  const rawGaps = row.gaps === null ? [] : row.gaps
  if (!Array.isArray(rawGaps)) return fail()
  const gaps = rawGaps.map(v => text(v)), expected = controls.filter(c => !c.minimum_met).map(c => c.id)
  if (new Set(gaps).size !== gaps.length || gaps.length !== expected.length || expected.some(id => !gaps.includes(id)) || row.satisfied !== (expected.length === 0)) return fail()
  return { version: 2, tenant_id: text(tenant), coverage: 'complete', assessment: 'not_assessed', from: start, to: end, total_count: count(row.total_count), controls, gaps, minimums_met: row.satisfied }
}
export const evidence = {
  async page(tenant: string, template: ReportTemplate, after = ''): Promise<EvidencePage> {
    if (!reportTemplates.includes(template)) return fail()
    const query = new URLSearchParams({ limit: '100' }); if (after) query.set('after', sequence(after))
    return parseEvidencePage(await api.get<unknown>(`/api/v2/audit/reports/${template}?${query}`), tenant, template, after)
  },
  async controls(tenant: string, from: string, to: string): Promise<ControlEvidenceReport> {
    timestamp(from); if (to && Date.parse(timestamp(to)) < Date.parse(from)) return fail()
    const query = new URLSearchParams({ from }); if (to) query.set('to', to)
    return parseControlEvidence(await api.get<unknown>(`/api/v2/soc2/evidence?${query}`, { acceptConflict: true }), tenant, from, to)
  },
}
export function describeEvidence(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 403) return 'Your account is not permitted to read this audit evidence.'
    if (error.status === 404) return 'The bounded evidence view is not enabled in this deployment.'
    if (error.status === 400) return 'Use a valid, ordered audit window with an explicit start time.'
    if (error.status === 422) return 'The window contains too many records. Narrow it; no complete evidence result was returned.'
    if (error.status === 503) return 'Audit evidence is currently unavailable.'
  }
  return 'Evidence data could not be verified. No result is shown.'
}
export function downloadEvidence(value: EvidencePage | ControlEvidenceReport, filename: string): void {
  const artifact = { artifact_type: 'records' in value ? 'tenant_report_page' : 'tenant_control_evidence_window', page_only: 'records' in value, record_projection: 'selected metadata only; not full signed records', ...value }
  const url = URL.createObjectURL(new Blob([JSON.stringify(artifact, null, 2)], { type: 'application/json' }))
  const link = document.createElement('a'); link.href = url; link.download = filename; document.body.appendChild(link); link.click(); link.remove()
  setTimeout(() => URL.revokeObjectURL(url), 0)
}


