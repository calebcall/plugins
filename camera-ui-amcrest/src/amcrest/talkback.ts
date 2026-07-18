import { Writable } from 'node:stream';

import type { AmcrestClient } from './api.js';

export interface TalkbackTarget {
  codec: 'aac' | 'pcm_alaw';
  contentType: 'Audio/AAC' | 'Audio/G.711A';
  sampleRate: number;
}

export function selectTalkbackTarget(deviceType: string | undefined): TalkbackTarget {
  const dt = (deviceType ?? '').toUpperCase();
  const isDahua = dt.startsWith('DH-') || dt.startsWith('DB');
  if (isDahua) {
    return { codec: 'pcm_alaw', contentType: 'Audio/G.711A', sampleRate: 8000 };
  }
  return { codec: 'aac', contentType: 'Audio/AAC', sampleRate: 16000 };
}

// Streams transcoded talkback audio to the camera's audio.cgi endpoint as a chunked POST.
export class AmcrestTalkback extends Writable {
  private started = false;

  constructor(
    private readonly client: AmcrestClient,
    private readonly channel: number,
    private readonly target: TalkbackTarget,
  ) {
    super();
  }

  // The camera controller pipes transcoder output here; the first write opens the POST.
  // Implemented in Step 3 wiring where the POST body is a stream fed by these writes.
}
