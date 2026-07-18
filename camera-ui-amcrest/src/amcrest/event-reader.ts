export function splitEventMultipart(chunk: string, boundary: string): string[] {
  const marker = `--${boundary}`;
  const spaced = `-- ${boundary}`;
  const normalized = chunk.split(spaced).join(marker);

  const parts = normalized.split(marker);
  const blobs: string[] = [];

  for (const part of parts) {
    const trimmed = part.trim();
    if (!trimmed || trimmed === '--') continue;

    // Drop MIME headers: the payload is after the first blank line.
    const sepIdx = trimmed.indexOf('\r\n\r\n');
    const payload = sepIdx !== -1 ? trimmed.substring(sepIdx + 4) : trimmed;
    const cleaned = payload
      .split(/\r?\n/)
      .filter((line) => line && !/^HTTP\/1\.[01] 200 OK$/.test(line.trim()))
      .join('\n')
      .trim();

    if (cleaned.includes('Code=')) {
      blobs.push(cleaned);
    }
  }

  return blobs;
}
