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
    'run_id', 'completed_at', 'statement_id', 'journal_position', 'scope_mode', 'transaction_pass',
    'statement_reference_count', 'book_reference_count', 'unreferenced_book_trades']) {
    if (response[key] !== undefined) expected[key] = response[key]
  }
  if (response.transaction_pass === 'ran') expected.transaction_comparison_basis = 'external_reference_membership'
  assert.deepEqual(JSON.parse(bytes), expected)
  await download.delete()
  return bytes
}

module.exports = { comparisonDownload }
