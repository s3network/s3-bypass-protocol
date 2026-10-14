#!/usr/bin/env node
// Builds an ISOLATED client+server config pair that shares one storage prefix.
//
// The point: the production bucket prefix is watched by the upstream server as
// well as by ours, so whichever one writes the session ACK first wins — a test
// against the shared prefix cannot prove our server is in the path. Pointing a
// fresh client AND our server at a dedicated prefix means only our server can
// pick the session up, so the test exercises our code on both ends.
//
// Secret hygiene: values are never printed, only booleans and key names.

import { chmodSync, readFileSync, writeFileSync } from 'node:fs'

// writeFileSync's mode only applies when it CREATES the file. Regenerating over
// an existing config leaves whatever mode that file had — so a regeneration
// could leave the storage credentials world-readable. chmod after the write is
// the only thing that actually enforces 0600.
function writeSecret(path, contents) {
  writeFileSync(path, contents, { mode: 0o600 })
  chmodSync(path, 0o600)
}

const [, , clientPath, prefix, outClient, outServer, listenPort, userPrefix] = process.argv
if (!clientPath || !prefix || !outClient || !outServer) {
  console.error('usage: build-isolated-pair.mjs <client.json> <prefix> <outClient.json> <outServer.json> [port] [userPrefix]')
  process.exit(2)
}

// The server side always runs ListenMultiUser: it scans "<user>/sessions" and
// refuses any prefix that is not listed in its `clients`. So the client has to
// write under a user directory and the server has to know that user — otherwise
// the session is never discovered. This mirrors how the real deployment issues
// per-user configs.
const user = userPrefix ?? 'b18-selftest'

const port = Number(listenPort ?? 8443)
// Number() on a non-number is NaN and JSON.stringify(NaN) is null: the config
// was written with "port": null and the problem only surfaced when the server
// refused to start.
if (!Number.isInteger(port) || port < 1 || port > 65535) {
  console.error(`FAIL: listen port ${JSON.stringify(listenPort)} is not a port number`)
  process.exit(1)
}

const client = JSON.parse(readFileSync(clientPath, 'utf8'))
const outbound = (client.outbounds ?? []).find((o) => o?.protocol === 'fedarisha')
if (!outbound?.settings?.storage) {
  console.error('FAIL: no fedarisha outbound with settings.storage in the client config')
  process.exit(1)
}

const storage = { ...outbound.settings.storage, prefix, sessionsDir: `${user}/sessions` }
const tuning = outbound.settings.tuning ?? {}

// Client: same shape as before, only the prefix differs.
const isoClient = {
  ...client,
  outbounds: client.outbounds.map((o) =>
    o?.protocol === 'fedarisha'
      ? { ...o, settings: { ...o.settings, storage } }
      : o,
  ),
}
writeSecret(outClient, JSON.stringify(isoClient, null, 2))

// Server: an inbound watching the same prefix.
const isoServer = {
  log: { loglevel: 'info' },
  inbounds: [
    {
      tag: 'fedarisha-in',
      listen: '127.0.0.1',
      port, // validated below, before anything is written
      protocol: 'fedarisha',
      settings: {
        storage: { ...storage, sessionsDir: 'sessions' },
        tuning,
        clients: [{ id: user, level: 0 }],
        userLevel: 0,
      },
    },
  ],
  outbounds: [{ tag: 'direct', protocol: 'freedom', settings: {} }],
}
writeSecret(outServer, JSON.stringify(isoServer, null, 2))

console.log('isolated pair written')
console.log('prefix used      :', prefix)
console.log('bucket present   :', Boolean(storage.bucket))
console.log('credentials      :', Boolean(storage.accessKey && storage.secretKey))
console.log('client config    :', outClient)
console.log('server config    :', outServer)
console.log('server listen    : 127.0.0.1:' + isoServer.inbounds[0].port)