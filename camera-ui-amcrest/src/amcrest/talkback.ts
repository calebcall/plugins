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
