import assert from 'node:assert/strict';
import { test } from 'node:test';

import { splitEventMultipart } from './event-reader.js';

const BODY = [
  '--myboundary',
  'Content-Type: text/plain',
  'Content-Length: 40',
  '',
  'Code=VideoMotion;action=Start;index=0',
  '--myboundary',
  'Content-Type: text/plain',
  'Content-Length: 39',
  '',
  'Code=VideoMotion;action=Stop;index=0',
  '--myboundary--',
].join('\r\n');

test('splits multipart body into event blobs', () => {
  const blobs = splitEventMultipart(BODY, 'myboundary');
  assert.equal(blobs.length, 2);
  assert.ok(blobs[0].includes('Code=VideoMotion;action=Start'));
  assert.ok(blobs[1].includes('Code=VideoMotion;action=Stop'));
});

test('handles the "-- boundary" (spaced) variant', () => {
  const spaced = BODY.replace(/--myboundary/g, '-- myboundary');
  const blobs = splitEventMultipart(spaced, 'myboundary');
  assert.equal(blobs.length, 2);
});
