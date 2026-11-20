const assert = require('node:assert/strict')

async function comparisonDownload(page, region, response) {
  const ready = page.waitForEvent('download')
  await region.getByRole('button', { name: 'Download comparison receipt' }).click()
  const download = await ready
  assert.equal(download.suggestedFilename(), 'custody-comparison-' + response.business_date + '.json')
  const stream = await download.createReadStream()
  const chunks = []
  let size = 0
  for await (const chunk of stream) {
    size += chunk.length
    assert(size < 64 * 1024)
    chunks.push(chunk)
  }
  const bytes = Buffer.concat(chunks)
  const expected = { format: 'kanz.custody-comparison.v1' }
  for (const key of ['tenant_id', 'portfolio_id', 'custodian_id', 'business_date', 'state', 'outcome',
    'run_id', 'completed_at', 'statement_id', 'journal_position', 'scope_mode', 'account_assignment_verified', 'transaction_pass',
    'statement_reference_count', 'book_reference_count', 'unreferenced_book_trades']) {
    if (response[key] !== undefined) expected[key] = response[key]
  }
  if (response.transaction_pass === 'ran') expected.transaction_comparison_basis = 'external_reference_membership'
  if (response.signed_statement_sha256 !== undefined) {
    if (response.execution_quantities !== undefined) {
      const { compared, unassessed, differences } = response.execution_quantities
      const amount = ({ coefficient, exponent }) => ({ coefficient, exponent })
      expected.execution_quantities = { compared, unassessed, differences: differences.map(row => ({
        external_ref: row.external_ref, instrument_id: row.instrument_id,
        ibor: amount(row.ibor), custodian: amount(row.custodian), difference: amount(row.difference),
      })) }
    }
    expected.lookup = 'retained_run'
    expected.signed_statement_sha256 = response.signed_statement_sha256
    expected.breaks = response.breaks.map(({ break_id, kind, key, values_state, ibor, custodian, difference }) =>
      ({ break_id, kind, key, values_state, ibor, custodian, difference }))
  }
  assert.deepEqual(JSON.parse(bytes), expected)
  await download.delete()
  return bytes
}

module.exports = { comparisonDownload }
