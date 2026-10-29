// Generates golden cases from pure modules at DeepSeek Harness
// 687f282e8625c6b8d11ff58211a90524be2e4317. Run with Node 24's native TS support:
// node internal/tui/secrets_upstream_generate.mjs /path/to/downloaded/modules
// The JSON fixture goes to stdout. Source files and original messages are read only.
import { readFile } from 'node:fs/promises'
import { createHash } from 'node:crypto'
import { resolve, join } from 'node:path'
import { pathToFileURL } from 'node:url'

const dir = resolve(process.argv[2] ?? '/private/tmp/dsgo-secret-mask-upstream')
const { CredentialScanner } = await import(pathToFileURL(join(dir, 'credential-scanner.ts')))
const { formatTables } = await import(pathToFileURL(join(dir, 'format-tables.ts')))
const { CREDENTIAL_FORMATS, PREFIXED, SK, JWT, UPPER_ALNUM } = await import(pathToFileURL(join(dir, 'credential-formats.ts')))
const scanner = new CredentialScanner(formatTables())
const byteOffset = (text, unit) => Buffer.byteLength(text.slice(0, unit), 'utf8')
const project = (text, match) => match ? {
  start: byteOffset(text, match.start),
  end: byteOffset(text, match.end),
  masked: match.data.masked,
} : null
function spans(text) {
  const out = []
  for (let from = 0; from < text.length;) {
    const match = scanner.match(text, from)
    if (!match) break
    out.push(project(text, match))
    if (match.end <= from) throw new Error('upstream matcher did not advance')
    from = match.end
  }
  return out
}
function masked(text, ranges) {
  const bytes = Buffer.from(text)
  let from = 0
  let result = ''
  for (const range of ranges) {
    result += bytes.subarray(from, range.start).toString('utf8') + range.masked
    from = range.end
  }
  return result + bytes.subarray(from).toString('utf8')
}
const rows = []
function add(text, unit = 0) {
  const ranges = spans(text)
  rows.push({ text, from: byteOffset(text, unit), first: project(text, scanner.match(text, unit)), spans: ranges, masked: masked(text, ranges) })
}

const tokens = []
for (const format of CREDENTIAL_FORMATS) {
  if (format.kind !== PREFIXED) continue
  let body = (format.run === UPPER_ALNUM ? 'A' : 'a').repeat(format.min)
  if (format.shape === SK) body = '1' + body.slice(1)
  if (format.shape === JWT) body += '.eyJ' + 'b'.repeat(8) + '.' + 'c'.repeat(8)
  const token = format.literal + body
  tokens.push(token)
  for (const prefix of ['', 'x', '_', '-', '中😀 ', '\n']) {
    for (const suffix of ['', '_', '-', 'x', ' ', '\r\n']) add(prefix + token + suffix)
  }
  add(`https://user:${token}@host`)
  add(`http://:${token}@host`)
}
for (const body of ['a'.repeat(19) + '1', 'a'.repeat(255) + '1', 'a'.repeat(256) + '1', 'a'.repeat(1000)]) add('sk-' + body)
for (const header of [7, 8, 2048, 2049]) add('eyJ' + 'a'.repeat(header) + '.eyJbbbbbbbb.cccccccc')
for (const label of ['', 'RSA ', 'OPENSSH ', 'A'.repeat(40), 'A'.repeat(41), 'rsa ']) {
  for (const ending of ['', '\n-----END PRIVATE KEY-----', '\r\n-----END RSA PRIVATE KEY-----', '-----END PRIVATE KEY-----']) {
    add(`-----BEGIN ${label}PRIVATE KEY-----\nabc😀\r\n秘密${ending}`)
    add(`-----BEGIN ${label}PRIVATE KEY-----abc😀${ending}`)
  }
}
for (const user of ['', 'user', '用户😀', 'a'.repeat(256), 'a'.repeat(257), '😀'.repeat(128), '😀'.repeat(129)]) {
  for (const scheme of ['http', 'https', 'ssh', 'x', 'HTTP', 'a+1', 'abcdefghijklmnoq', 'abcdefghijklmnoqr']) {
    const text = `前😀 ${scheme}://${user}:密😀码:another@host/path`
    for (let unit = 0; unit <= text.length;) {
      add(text, unit)
      if (unit === text.length) break
      unit += text.codePointAt(unit) > 0xffff ? 2 : 1
    }
  }
}
const block = '-----BEGIN PRIVATE KEY-----\nabc😀\n-----END PRIVATE KEY-----'
add(tokens[0] + block)
add(block + tokens[0])
add('https://user:' + tokens[0] + '@host ' + block)
add('password=plain api_key=plain TOKEN=ordinary-secret')
add('http://user:pass?query@host http://user:pass#fragment@host')

let seed = 0x4d41534b
const random = n => { seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0; return seed % n }
const pieces = ['', ' ', '\n', '\r\n', '😀', '中文', 'x', '_', '-', ':', '@', '?', '#', '/', '://', 'http://', 'user:', '-----BEGIN ', 'PRIVATE KEY-----', '\n-----END PRIVATE KEY-----', 'sk-', 'a'.repeat(25), '1', '.eyJ', 'eyJabcdefgh', ...tokens]
for (let index = 0; index < 400; index++) {
  let text = ''
  for (let count = 2 + random(10); count > 0; count--) text += pieces[random(pieces.length)]
  add(text)
  const boundaries = [0]
  for (let unit = 0; unit < text.length;) { unit += text.codePointAt(unit) > 0xffff ? 2 : 1; boundaries.push(unit) }
  add(text, boundaries[random(boundaries.length)])
}
const checksums = {}
for (const name of ['credential-formats', 'credential-masks', 'credential-scanner', 'format-tables', 'secret-rule']) {
  checksums[name + '.ts'] = createHash('sha256').update(await readFile(join(dir, name + '.ts'))).digest('hex')
}
process.stdout.write(JSON.stringify({ sha: '687f282e8625c6b8d11ff58211a90524be2e4317', checksums, cases: rows }) + '\n')
