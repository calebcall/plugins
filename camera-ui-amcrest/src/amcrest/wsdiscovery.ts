// ONVIF WS-Discovery (SOAP over UDP multicast, 239.255.255.250:3702).
//
// Many Amcrest units answer ONVIF WS-Discovery but NOT the Dahua DHIP probe
// (see discovery.ts), so this is the primary discovery mechanism. It is kept
// dependency-free (no ONVIF library) so the plugin stays self-contained.

import { randomUUID } from 'node:crypto';
import { createSocket } from 'node:dgram';
import { networkInterfaces } from 'node:os';

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

const PROBE_RESEND_MS = 1200;

function localIPv4Addresses(): string[] {
  const nifs = networkInterfaces();
  const addrs: string[] = [];
  for (const list of Object.values(nifs)) {
    for (const ni of list ?? []) {
      if (ni.family === 'IPv4' && !ni.internal) addrs.push(ni.address);
    }
  }
  return addrs;
}

export async function discoverWs(timeoutMs: number, logger: WsDiscoveryLogger): Promise<WsDiscovered[]> {
  return new Promise((resolvePromise) => {
    const found = new Map<string, WsDiscovered>();
    const seen = new Set<string>();
    const sockets: ReturnType<typeof createSocket>[] = [];
    const senders: (() => void)[] = [];
    const cleanups: (() => void)[] = [];
    let finished = false;

    // Probe from every non-internal IPv4 interface; a single default-interface
    // probe misses cameras reachable only via another NIC/VLAN/bridge.
    const addrs = localIPv4Addresses();
    logger.log(`WS-Discovery: probing ${addrs.length} interface(s): ${addrs.length ? addrs.join(', ') : '(default only)'}`);
    const bindTargets: (string | undefined)[] = addrs.length ? addrs : [undefined];

    const timer = setTimeout(finish, timeoutMs);
    cleanups.push(() => clearTimeout(timer));

    function finish() {
      if (finished) return;
      finished = true;
      for (const c of cleanups) c();
      for (const s of sockets) {
        try {
          s.close();
        } catch {
          // ignore
        }
      }
      resolvePromise(Array.from(found.values()));
    }

    function handleMessage(msg: Buffer, rinfo: { address: string }) {
      const src = rinfo.address;
      const device = parseWsProbeMatch(msg.toString('utf8'));
      if (!device) {
        // Diagnostic: a reply we received but could not parse (unexpected format).
        if (!seen.has(src)) {
          seen.add(src);
          logger.log(`WS-Discovery: unparsed reply from ${src} (len=${msg.length}): ${msg.toString('utf8').slice(0, 200).replace(/\s+/g, ' ')}`);
        }
        return;
      }
      if (seen.has(device.ip)) return;
      seen.add(device.ip);
      const amcrest = isAmcrestDevice(device.scopes);
      logger.log(
        `WS-Discovery: ip=${device.ip} (via ${src}) manufacturer=${device.manufacturer ?? '?'} name=${device.name ?? '?'} hardware=${device.hardware ?? '?'} amcrest=${amcrest}`,
      );
      if (amcrest) {
        found.set(device.ip, device);
      }
    }

    for (const addr of bindTargets) {
      const socket = createSocket({ type: 'udp4', reuseAddr: true });
      sockets.push(socket);
      // One socket failing (e.g. bind conflict) must not abort the whole scan.
      socket.on('error', (err) => logger.debug(`WS-Discovery socket error (${addr ?? '*'}):`, err));
      socket.on('message', handleMessage);
      socket.bind(addr ? { address: addr, port: 0 } : { port: 0 }, () => {
        try {
          socket.setBroadcast(true);
          if (addr) socket.setMulticastInterface(addr);
        } catch (err) {
          logger.debug(`WS-Discovery setup failed (${addr ?? '*'}):`, err);
        }
        senders.push(() => {
          try {
            socket.send(Buffer.from(buildWsDiscoveryProbe(randomUUID()), 'utf8'), WSD_PORT, WSD_ADDR);
          } catch (err) {
            logger.debug(`WS-Discovery send failed (${addr ?? '*'}):`, err);
          }
        });
      });
    }

    // Resend across the window (dedup handles repeats); the first tick gives the
    // per-interface binds time to register their senders.
    const sendAll = () => senders.forEach((s) => s());
    const firstSend = setTimeout(sendAll, 100);
    const resend = setInterval(sendAll, PROBE_RESEND_MS);
    cleanups.push(() => clearTimeout(firstSend), () => clearInterval(resend));
  });
}
