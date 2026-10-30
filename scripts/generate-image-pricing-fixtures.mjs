// Execute the pinned upstream's pure TypeScript functions, not a second port.
// node scripts/generate-image-pricing-fixtures.mjs <upstream-root> <output.json>
import { readFileSync, writeFileSync } from 'node:fs'
import { stripTypeScriptTypes } from 'node:module'
import { createHash } from 'node:crypto'
import { join } from 'node:path'

const [root, output] = process.argv.slice(2)
if (!root || !output) throw new Error('upstream root and output path required')
const paths = [
  'packages/attachment/attachment/src/request-projection.ts',
  'packages/llm/llm-deepseek/src/image-tokens.ts',
]
const sources = paths.map(path => ({ path, text: readFileSync(join(root, path), 'utf8') }))
const code = sources.map(({ text }) => stripTypeScriptTypes(text, { mode: 'transform' })
  .split('\n').filter(line => !line.startsWith('import ')).join('\n')).join('\n')
let upstream
try {
  upstream = await import(`data:text/javascript;base64,${Buffer.from(code).toString('base64')}`)
} catch (error) {
  // Avoid dumping an entire data URL into failed import stack traces.
  throw new Error(`Unable to load pinned pure functions: ${error.message}`)
}
const dimensions = [[1, 1], [1, 8192], [8192, 1], [14, 14], [544, 544], [543, 545],
  [4096, 4096], [8192, 8192], [2048, 2048], [1600, 1200], [8192, 64], [64, 8192]]
let random = 687282
for (let i = 0; i < 500; i++) {
  random = (Math.imul(random, 1664525) + 1013904223) >>> 0
  const width = random % 8192 + 1
  random = (Math.imul(random, 1664525) + 1013904223) >>> 0
  dimensions.push([width, random % 8192 + 1])
}
const cases = dimensions.map(([width, height]) => {
  const grid = upstream.deepSeekRequestImageDimensions(width, height)
  const target = Math.max(grid.width, grid.height) > 4096
    ? upstream.longEdgeDimensions(width, height, 4096) : grid
  return { width, height, target, tokens: upstream.deepSeekImageTokens(target.width, target.height),
    sourceTokens: upstream.deepSeekImageTokens(width, height) }
})
writeFileSync(output, JSON.stringify({
  upstream: '687f282e8625c6b8d11ff58211a90524be2e4317',
  sources: sources.map(({ path, text }) => ({ path, sha256: createHash('sha256').update(text).digest('hex') })),
  cases,
}, null, 2) + '\n')
