#!/usr/bin/env node
// Builds a server-shaped fedarisha config from the client's own config.
//
// The client config already carries the storage block (bucket, endpoint,
// credentials) the server needs — both ends talk to the same bucket. This
// script copies that block into an inbound so our own server can be run
// against the same backend.
//
// Secret hygiene: nothing is printed. Only booleans and key NAMES are
// reported, never values. The server config is written with mode 600 and that
// mode is enforced on every write — see writeSecret.

import { chmodSync, readFileSync, writeFileSync } from 'node:fs'

const [, , clientPath, serverPath, listenPort] = process.argv
if (!clientPath || !serverPath) {
  console.error('usage: build-server-config.mjs <client.json> <server.json> [listenPort]')
  process.exit(2)
}

const client = JSON.parse(readFileSync(clientPath, 'utf8'))

// Pull the storage block out of the fedarisha outbound.
const outbound = (client.outbounds ?? []).find((o) => o?.protocol === 'fedarisha')
if (!outbound) {
  console.error('FAIL: client config has no outbound with protocol "fedarisha"')
  process.exit(1)
}
const storage = outbound.settings?.storage
if (!storage) {
  console.error('FAIL: fedarisha outbound has no settings.storage')
  process.exit(1)
}

const required = ['bucket', 'endpoint', 'accessKey', 'secretKey']
const missing = required.filter((k) => !storage[k])
if (missing.length > 0) {
  // Report the NAMES only — never the values.
  console.error(`FAIL: storage is missing required keys: ${missing.join(', ')}`)
  process.exit(1)
}

// The server always starts through ListenMultiUser: it scans "<user>/sessions"
// and refuses every session whose prefix is not listed in `clients`. A config
// with an empty clients list therefore rejects EVERY session it sees, deletes
// the hello file on the way out, and reports nothing wrong with itself. The user
// id is the first path segment of the client's own storage prefix.
const userId = (storage.prefix ?? '').split('/').filter(Boolean)[0]
if (!userId) {
  console.error(
    'FAIL: cannot work out the user id — storage.prefix is missing or empty.',
  )
  console.error('      A fedarisha server started with ListenMultiUser needs at least')
  console.error('      one entry in clients, and the id is the first segment of the')
  console.error('      prefix the client writes under (e.g. "b18-/").')
  process.exit(1)
}

const port = Number(listenPort ?? 8443)
// Number() on something that is not a number is NaN, and JSON.stringify(NaN) is
// null: the config was written with "port": null and the failure surfaced only
// when the server refused to start.
if (!Number.isInteger(port) || port < 1 || port > 65535) {
  console.error(`FAIL: listen port ${JSON.stringify(listenPort)} is not a port number`)
  process.exit(1)
}

const tuning = outbound.settings?.tuning ?? {}

const server = {
  log: { loglevel: 'info' },
  inbounds: [
    {
      tag: 'fedarisha-in',
      listen: '127.0.0.1',
      port,
      protocol: 'fedarisha',
      settings: {
        storage: { ...storage },
        tuning: { ...tuning },
        clients: [{ id: userId, level: 0 }],
        userLevel: 0,
      },
    },
  ],
  outbounds: [
    { tag: 'direct', protocol: 'freedom', settings: {} },
  ],
}

// writeFileSync's mode only applies when it CREATES the file. Regenerating over
// a config that already exists leaves whatever mode that file had — so a file
// written once by an older version of this script, or copied from somewhere
// else, kept its 0644 and the storage credentials in it stayed world-readable.
// chmod after the write is the only thing that actually enforces it.
function writeSecret(path, contents) {
  writeFileSync(path, contents, { mode: 0o600 })
  chmodSync(path, 0o600)
}

writeSecret(serverPath, JSON.stringify(server, null, 2))

// Booleans only — the point is to prove the shape without leaking anything.
console.log('server config written:', serverPath)
console.log('storage keys present :', Object.keys(storage).sort().join(', '))
console.log('credentials present  :', Boolean(storage.accessKey && storage.secretKey))
console.log('inbound listen       : 127.0.0.1:' + server.inbounds[0].port)
console.log('client user id       :', userId)
console.log('tuning carried over  :', Object.keys(tuning).length > 0)