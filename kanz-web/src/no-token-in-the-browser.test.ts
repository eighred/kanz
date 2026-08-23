import { describe, expect, it } from 'vitest'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative, sep } from 'node:path'

// THE TOKEN NEVER REACHES THE BROWSER, AND THAT IS APP-WIDE (#371).
//
// # Why this is a guard and not a paragraph
//
// The BFF holds the token; the browser holds only an opaque, httpOnly session
// id. #371 states the reason and states it as non-negotiable: a token here
// carries `kanz-trader` (moves capital) or `kanz-operator` (writes venue keys,
// drains nodes), so a token in JS-reachable storage turns ONE XSS ANYWHERE IN
// THIS APP into a full-authority credential theft — and a silent one. With an
// httpOnly cookie the same XSS is a session-riding problem bounded by the
// session.
//
// `src/api/client.test.ts` already asserts that THE CLIENT sends no
// Authorization header. That is one module. It cannot stop a new view, store or
// composable from calling `localStorage.setItem('token', …)` or attaching a
// header of its own — and the property is about the whole application, not
// about one file. This is the difference between a guard that holds the
// property and one that holds an example of it.
//
// # Comments are stripped before matching
//
// The rule's own documentation names every identifier it forbids — the sentence
// you are reading names four of them, and `src/api/client.ts` carries "the
// Authorization header below is the design, not an omission". A guard that
// fires on the prose explaining it is one people learn to route around, so the
// scan strips comments and string-free code is what is matched.
//
// # What it does not claim
//
// It does not prove the deployed bundle is free of a token: a dependency could
// do this, and a build step could inject one. It holds the property for the
// source in this repository, which is where a regression would be introduced by
// someone working here.

const SRC = join(process.cwd(), 'src')

/** Source files the SPA is built from. Tests are excluded: they legitimately
 *  name the forbidden APIs to assert the app does not use them. */
function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) {
      if (entry === 'node_modules' || entry === 'dist') continue
      sourceFiles(full, out)
      continue
    }
    if (!/\.(ts|vue|js)$/.test(entry)) continue
    if (/\.test\.ts$/.test(entry)) continue
    out.push(full)
  }
  return out
}

/** Remove comments so the guard cannot fire on the documentation of its own
 *  rule. Handles `//`, block comments and Vue's `<!-- -->`. String literals are
 *  left alone: a token written into a string is exactly what this looks for. */
function stripComments(text: string): string {
  return text
    .replace(/\/\*[\s\S]*?\*\//g, ' ')
    .replace(/<!--[\s\S]*?-->/g, ' ')
    .replace(/(^|[^:])\/\/.*$/gm, '$1')
}

/** The browser-observable places a credential could be put, and the header the
 *  SPA must never set. Each carries the consequence, printed on failure. */
const FORBIDDEN: ReadonlyArray<{ pattern: RegExp; what: string; why: string }> = [
  {
    pattern: /\blocalStorage\b/,
    what: 'localStorage',
    why: 'readable by any script on the origin and survives the tab. An XSS reads it once and holds the credential after the session ends.',
  },
  {
    pattern: /\bsessionStorage\b/,
    what: 'sessionStorage',
    why: 'readable by any script on the origin. Shorter-lived than localStorage and exactly as exfiltratable.',
  },
  {
    pattern: /\bdocument\s*\.\s*cookie\b/,
    what: 'document.cookie',
    why: 'the session cookie is httpOnly precisely so this returns nothing. Reading or writing cookies from JS means a credential that is NOT httpOnly.',
  },
  {
    pattern: /\bindexedDB\b/,
    what: 'indexedDB',
    why: 'origin-scoped, script-readable and durable — the same exposure as localStorage with more capacity.',
  },
  {
    pattern: /['"`]Authorization['"`]\s*:/,
    what: 'an Authorization header',
    why: 'the BFF attaches the token as the session\'s caller. An SPA that sets this header is an SPA that HAS a token.',
  },
  {
    pattern: /\.\s*setRequestHeader\s*\(\s*['"`]Authorization/i,
    what: 'an Authorization header via XHR',
    why: 'same as above, one API further down.',
  },
]

describe('the token never reaches the browser', () => {
  const files = sourceFiles(SRC)

  // NON-VACUITY. A broken walk reports a clean app for the same reason an empty
  // one would, and this guard would pass having read nothing.
  it('scans the application source', () => {
    expect(files.length).toBeGreaterThan(20)
  })

  it('stores no credential in browser-observable storage and sets no Authorization header', () => {
    const offences: string[] = []
    for (const file of files) {
      const code = stripComments(readFileSync(file, 'utf8'))
      for (const rule of FORBIDDEN) {
        if (!rule.pattern.test(code)) continue
        offences.push(
          `${relative(process.cwd(), file).split(sep).join('/')} uses ${rule.what} — ${rule.why}`,
        )
      }
    }
    expect(
      offences,
      'The BFF holds the token; the browser holds only an opaque httpOnly session id (#371).\n' +
        'A token in JS-reachable storage turns one XSS anywhere in this app into a full-authority\n' +
        'credential theft, and this application signs in users who can move capital and write\n' +
        'venue keys. If a screen needs state across reloads, ask the BFF for it.\n\n' +
        offences.join('\n'),
    ).toEqual([])
  })

  // The stripper is load-bearing: without it this guard fires on the comment in
  // src/api/client.ts that explains the rule. If it ever stops removing
  // comments, the guard becomes noise and gets deleted.
  it('strips comments before matching, so the rule does not fire on its own documentation', () => {
    expect(stripComments('// we never touch localStorage here')).not.toMatch(/localStorage/)
    expect(stripComments('/* Authorization: never */')).not.toMatch(/Authorization/)
    expect(stripComments('<!-- no document.cookie -->')).not.toMatch(/document\.cookie/)
    // ...and does NOT strip real code, or the guard would pass on everything.
    expect(stripComments('localStorage.setItem("t", tok)')).toMatch(/localStorage/)
    // A URL's // must survive: stripping it would truncate the line and hide
    // whatever follows on it.
    expect(stripComments('const u = "https://example.test/x"; localStorage.clear()')).toMatch(
      /localStorage/,
    )
  })
})
