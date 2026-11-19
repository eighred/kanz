import { api } from './client'
import { exactRatio } from './exact-ratio'
import { isTimestamp } from './timestamp'

export interface ScenarioAsset {
  instrument: string
  current: string
  price: string
  expectedReturn: string
}

export interface Scenario {
  portfolio: string
  currency: string
  nav: string
  threshold: string
  assets: ScenarioAsset[]
}

type Ratio = ReturnType<typeof exactRatio>

function invalid(message: string): never { throw new Error(message) }
function unconfirmed(): never { return invalid('Optimization returned an unconfirmed proposal.') }

function identifier(value: string): boolean {
  return value.trim() !== '' && value.trim() === value && !value.includes('\0') && new TextEncoder().encode(value).length <= 256
}

function scenarioRatio(value: string, label: string): Ratio {
  try { return exactRatio(value) } catch { return invalid(`Enter an exact decimal or fraction for ${label}.`) }
}

function sumWeights(values: Iterable<Ratio>) {
  let numerator = 0n, denominator = 1n
  for (const weight of values) {
    numerator = numerator * weight.denominator + weight.numerator * denominator
    denominator *= weight.denominator
  }
  return { empty: numerator === 0n, invested: numerator === denominator }
}

function requestFor(form: Scenario) {
  if (!identifier(form.portfolio) || !/^[A-Z]{3,12}$/.test(form.currency)) return invalid('Enter a portfolio ID and uppercase valuation currency.')
  if (scenarioRatio(form.nav, 'NAV').numerator <= 0n) return invalid('NAV must be positive.')
  const band = scenarioRatio(form.threshold, 'the threshold')
  if (band.numerator < 0n || band.numerator > band.denominator) return invalid('The weight threshold must be between zero and one.')
  if (!form.assets.length || form.assets.length > 128) return invalid('Enter between 1 and 128 instruments.')
  const ids = new Set<string>(), weights: Ratio[] = []
  const instruments: string[] = [], expected_returns: number[] = []
  const current_weights: Record<string, string> = Object.create(null), prices: Record<string, string> = Object.create(null)
  for (const asset of form.assets) {
    const id = asset.instrument
    if (!identifier(id) || ids.has(id.toLowerCase())) return invalid('Each instrument must have a distinct ID.')
    ids.add(id.toLowerCase())
    const weight = scenarioRatio(asset.current, 'each current weight')
    if (weight.numerator < 0n || weight.numerator > weight.denominator) return invalid('Current weights must be between zero and one.')
    if (scenarioRatio(asset.price, 'each price').numerator <= 0n) return invalid('Prices must be positive.')
    const estimate = Number(asset.expectedReturn)
    if (asset.expectedReturn.length > 100 || !/^-?(?:\d+(?:\.\d+)?|\.\d+)(?:e[+-]?\d+)?$/i.test(asset.expectedReturn) ||
        !Number.isFinite(estimate) || (estimate === 0 && /[1-9]/.test(asset.expectedReturn.split(/e/i)[0]!))) return invalid('Enter a finite expected return for each instrument.')
    weights.push(weight)
    instruments.push(id)
    current_weights[id] = asset.current
    prices[id] = asset.price
    expected_returns.push(estimate)
  }
  const book = sumWeights(weights)
  if (!book.empty && !book.invested) return invalid('Current weights must total zero or one.')
  return { portfolio_id: form.portfolio, currency: form.currency, nav: form.nav, threshold: form.threshold,
    instruments, current_weights, prices, expected_returns, objective: { Type: 0, RiskAversion: 0, RiskFreeRate: 0 } }
}

function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return unconfirmed()
  return value as Record<string, unknown>
}
function responseRatio(value: unknown): Ratio {
  try { return exactRatio(value) } catch { return unconfirmed() }
}
function compare(a: Ratio, b: Ratio) { return a.numerator * b.denominator - b.numerator * a.denominator }

function weights(value: unknown, ids: string[]) {
  const map = object(value)
  if (Object.keys(map).length !== ids.length) return unconfirmed()
  return new Map(ids.map(id => {
    if (!Object.hasOwn(map, id)) return unconfirmed()
    const weight = responseRatio(map[id])
    if (weight.numerator < 0n || weight.numerator > weight.denominator) return unconfirmed()
    return [id, weight]
  }))
}

function parseProposal(value: unknown, request: ReturnType<typeof requestFor>) {
  const reply = object(value), goal = object(reply.Objective)
  if (reply.SchemaVersion !== 2 || reply.ReadOnly !== true || reply.PortfolioID !== request.portfolio_id ||
      reply.Currency !== request.currency || !isTimestamp(reply.AsOf) || goal.Type !== 0 || goal.RiskAversion !== 0 ||
      goal.RiskFreeRate !== 0 || typeof reply.ExpectedReturn !== 'number' || !Number.isFinite(reply.ExpectedReturn) ||
      reply.ExpectedRisk !== null || reply.CovarianceQuality !== 'UNCHECKED' ||
      reply.TargetWeightPolicy !== 'decimal-12-nearest-even; budget-residual-largest-absolute-weight; exact-post-check') return unconfirmed()
  const verdict = reply.MandateStatus
  const violations = reply.Violations === null ? [] : reply.Violations
  if ((verdict !== 'UNCHECKED' && verdict !== 'FEASIBLE' && verdict !== 'INFEASIBLE') || !Array.isArray(violations) ||
      !violations.every((v): v is string => typeof v === 'string') || (verdict !== 'INFEASIBLE' && violations.length)) return unconfirmed()
  const targets = weights(reply.Targets, request.instruments), evaluated = weights(reply.EvaluatedWeights, request.instruments)
  const rows = reply.Trades === null ? [] : reply.Trades
  const candidateBudget = sumWeights(evaluated.values())
  if (!Array.isArray(rows) || rows.length > request.instruments.length || !sumWeights(targets.values()).invested ||
      !(candidateBudget.invested || (candidateBudget.empty && rows.length === 0))) return unconfirmed()
  const changed = new Set<string>(), nav = responseRatio(request.nav)
  const threshold = responseRatio(request.threshold)
  let grossNumerator = 0n, grossDenominator = 1n
  const trades = rows.map(raw => {
    const trade = object(raw), id = trade.InstrumentID
    if (typeof id !== 'string' || !targets.has(id) || changed.has(id) || (trade.Side !== 1 && trade.Side !== 2)) return unconfirmed()
    changed.add(id)
    const before = responseRatio(trade.CurrentWeight), after = responseRatio(trade.TargetWeight)
    if (compare(before, responseRatio(request.current_weights[id])) !== 0n || compare(after, targets.get(id)!) !== 0n ||
        compare(after, evaluated.get(id)!) !== 0n) return unconfirmed()
    const difference = compare(after, before), magnitude = difference < 0n ? -difference : difference
    const quantity = responseRatio(trade.Quantity), notional = responseRatio(trade.Notional), price = responseRatio(request.prices[id])
    // Check the returned trade against the operator's NAV, price and weight change without rounding.
    const weightDenominator = after.denominator * before.denominator
    if (magnitude * threshold.denominator <= threshold.numerator * weightDenominator || (difference > 0n) !== (trade.Side === 1) || quantity.numerator <= 0n || notional.numerator <= 0n ||
        magnitude * nav.numerator * notional.denominator !== weightDenominator * nav.denominator * notional.numerator ||
        quantity.numerator * price.numerator * notional.denominator !== notional.numerator * quantity.denominator * price.denominator) return unconfirmed()
    grossNumerator = grossNumerator * weightDenominator + magnitude * grossDenominator
    grossDenominator *= weightDenominator
    return { id, side: trade.Side === 1 ? 'Buy' : 'Sell', quantity: quantity.text, notional: notional.text }
  })
  const book = request.instruments.map(id => {
    const current = responseRatio(request.current_weights[id]), candidate = evaluated.get(id)!
    const target = targets.get(id)!
    if (!changed.has(id)) {
      const difference = compare(target, current), magnitude = difference < 0n ? -difference : difference
      if (compare(current, candidate) !== 0n || magnitude * threshold.denominator >
          threshold.numerator * target.denominator * current.denominator) return unconfirmed()
    }
    return { id, current: current.text, target: target.text, candidate: candidate.text }
  })
  const turnover = responseRatio(reply.Turnover)
  if (turnover.numerator * 2n * grossDenominator !== turnover.denominator * grossNumerator) return unconfirmed()
  return { portfolio: request.portfolio_id, currency: request.currency, asOf: reply.AsOf, verdict, violations,
    book, trades, turnover: turnover.text, expectedReturn: reply.ExpectedReturn }
}

export type ModelProposal = ReturnType<typeof parseProposal>

export async function reviewModelProposal(form: Scenario, signal: AbortSignal): Promise<ModelProposal> {
  const request = requestFor(form)
  const response = await api.post<unknown>('/api/v2/model-portfolios/propose', request, 200, signal)
  return parseProposal(response, request)
}
