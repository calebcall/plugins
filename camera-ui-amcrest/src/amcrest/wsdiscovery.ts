// ONVIF WS-Discovery (SOAP over UDP multicast, 239.255.255.250:3702).
//
// Many Amcrest units answer ONVIF WS-Discovery but NOT the Dahua DHIP probe
// (see discovery.ts), so this is the primary discovery mechanism. It is kept
// dependency-free (no ONVIF library) so the plugin stays self-contained.

import { randomUUID } from 'node:crypto';
import { createSocket } from 'node:dgram';

export interface WsDiscovered {
  ip: string;
  manufacturer?: string;
  name?: string;
  hardware?: string;
  scopes: string[];
}

const WSD_ADDR = '239.255.255.250';
const WSD_PORT = 3702;

// Amcrest/Dahua hardware/name prefixes seen in ONVIF scopes, used as a fallback
// when the manufacturer scope is absent or generic.
const AMCREST_HW_RE = /^(ip[0-9]|ipc|ip2m|ip3m|ip4m|ip5m|ip8m|ad[0-9]|amc|ash|asd|dh-|dahua)/i;

export function buildWsDiscoveryProbe(messageId: string): string {
  return (
    '<?xml version="1.0" encoding="UTF-8"?>' +
    '<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope"' +
    ' xmlns:w="http://schemas.xmlsoap.org/ws/2004/08/addressing"' +
    ' xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"' +
    ' xmlns:dn="http://www.onvif.org/ver10/network/wsdl">' +
    '<e:Header>' +
    `<w:MessageID>urn:uuid:${messageId}</w:MessageID>` +
    '<w:To e:mustUnderstand="true">urn:schemas-xmlsoap-org:ws:2005:04:discovery</w:To>' +
    '<w:Action e:mustUnderstand="true">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</w:Action>' +
    '</e:Header>' +
    '<e:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></e:Body>' +
    '</e:Envelope>'
  );
}

// Extract the text content of the first element whose local name matches `tag`,
// ignoring any XML namespace prefix.
function firstTag(xml: string, tag: string): string | undefined {
  const re = new RegExp(`<[A-Za-z0-9_.]*:?${tag}[^>]*>([\\s\\S]*?)</[A-Za-z0-9_.]*:?${tag}>`, 'i');
  const m = re.exec(xml);
  return m ? m[1].trim() : undefined;
}

export function scopeValue(scopes: string[], key: string): string | undefined {
  const prefix = `onvif://www.onvif.org/${key}/`.toLowerCase();
  const s = scopes.find((x) => x.toLowerCase().startsWith(prefix));
  if (!s) return undefined;
  try {
    return decodeURIComponent(s.substring(prefix.length));
  } catch {
    return s.substring(prefix.length);
  }
}

export function isAmcrestDevice(scopes: string[]): boolean {
  const blob = scopes.join(' ').toLowerCase();
  if (blob.includes('amcrest') || blob.includes('dahua')) return true;
  const hardware = scopeValue(scopes, 'hardware');
  if (hardware && AMCREST_HW_RE.test(hardware)) return true;
  const name = scopeValue(scopes, 'name');
  if (name && AMCREST_HW_RE.test(name)) return true;
  return false;
}

export function parseWsProbeMatch(xml: string): WsDiscovered | undefined {
  const xaddrs = firstTag(xml, 'XAddrs');
  const scopesRaw = firstTag(xml, 'Scopes');
  if (!xaddrs && !scopesRaw) return undefined;

  let ip: string | undefined;
  if (xaddrs) {
    const urlMatch = /https?:\/\/([^/:\s]+)/i.exec(xaddrs);
    if (urlMatch) ip = urlMatch[1];
  }
  if (!ip) return undefined;

  const scopes = scopesRaw ? scopesRaw.split(/\s+/).filter(Boolean) : [];
  return {
    ip,
    manufacturer: scopeValue(scopes, 'manufacturer'),
    name: scopeValue(scopes, 'name'),
    hardware: scopeValue(scopes, 'hardware'),
    scopes,
  };
}

export interface WsDiscoveryLogger {
  debug: (...a: unknown[]) => void;
  log: (...a: unknown[]) => void;
}

export async function discoverWs(timeoutMs: number, logger: WsDiscoveryLogger): Promise<WsDiscovered[]> {
  return new Promise((resolvePromise) => {
    const found = new Map<string, WsDiscovered>();
    const socket = createSocket({ type: 'udp4', reuseAddr: true });
    let finished = false;

    const timer = setTimeout(finish, timeoutMs);

    function finish() {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      try {
        socket.close();
      } catch {
        // ignore
      }
      resolvePromise(Array.from(found.values()));
    }

    socket.on('error', (err) => {
      logger.debug('WS-Discovery socket error:', err);
      finish();
    });

    socket.on('message', (msg) => {
      const device = parseWsProbeMatch(msg.toString('utf8'));
      if (!device) return;
      const amcrest = isAmcrestDevice(device.scopes);
      logger.log(
        `WS-Discovery: ip=${device.ip} manufacturer=${device.manufacturer ?? '?'} name=${device.name ?? '?'} hardware=${device.hardware ?? '?'} amcrest=${amcrest}`,
      );
      if (amcrest && !found.has(device.ip)) {
        found.set(device.ip, device);
      }
    });

    socket.bind(() => {
      try {
        const probe = Buffer.from(buildWsDiscoveryProbe(randomUUID()), 'utf8');
        socket.send(probe, WSD_PORT, WSD_ADDR);
      } catch (err) {
        logger.debug('WS-Discovery send failed:', err);
      }
    });
  });
}
