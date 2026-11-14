import { tick } from 'svelte';
import type {
  AudioDevice,
  AudioDeviceSummary,
  ClusterNodesResponse,
  ClusterDevicesResponse,
  MatrixRoute,
  RoomInfo,
  SystemStatus,
} from '$lib/api/types';

/**
 * Deterministic poller awaiting Svelte 5 microtask queue tick()
 * without arbitrary sleeps or brittle timeouts.
 */
export async function waitForStore<T>(
  storeGetter: () => T,
  predicate: (value: T) => boolean,
  timeoutMs = 4000,
  pollIntervalMs = 15
): Promise<T> {
  const start = Date.now();
  while (true) {
    await tick();
    const val = storeGetter();
    if (predicate(val)) {
      return val;
    }
    if (Date.now() - start > timeoutMs) {
      throw new Error(`waitForStore timed out after ${timeoutMs}ms waiting for condition`);
    }
    await new Promise((resolve) => setTimeout(resolve, pollIntervalMs));
  }
}

/**
 * MockLocalStorage implements standard Web Storage interface with
 * disk snapshot capture and simulated crash/reboot capabilities.
 */
export class MockLocalStorage implements Storage {
  private memoryStore: Map<string, string> = new Map();
  private diskSnapshot: Record<string, string> = {};

  get length(): number {
    return this.memoryStore.size;
  }

  clear(): void {
    this.memoryStore.clear();
  }

  getItem(key: string): string | null {
    return this.memoryStore.has(key) ? this.memoryStore.get(key)! : null;
  }

  key(index: number): string | null {
    const keys = Array.from(this.memoryStore.keys());
    return keys[index] ?? null;
  }

  removeItem(key: string): void {
    this.memoryStore.delete(key);
  }

  setItem(key: string, value: string): void {
    this.memoryStore.set(key, String(value));
    // Mirror to persistent disk snapshot on successful write
    this.diskSnapshot[key] = String(value);
  }

  snapshot(): Record<string, string> {
    const snap: Record<string, string> = {};
    for (const [k, v] of this.memoryStore.entries()) {
      snap[k] = v;
    }
    return snap;
  }

  restoreSnapshot(snap: Record<string, string>): void {
    this.memoryStore.clear();
    for (const [k, v] of Object.entries(snap)) {
      this.memoryStore.set(k, v);
    }
    this.diskSnapshot = { ...snap };
  }

  simulateCrash(): void {
    // A crash immediately halts process and tears down volatile memory
    this.memoryStore.clear();
  }

  simulateReboot(preserveDisk = true): void {
    this.memoryStore.clear();
    if (preserveDisk) {
      for (const [k, v] of Object.entries(this.diskSnapshot)) {
        this.memoryStore.set(k, v);
      }
    } else {
      this.diskSnapshot = {};
    }
  }

  install(): () => void {
    const originalLocal = globalThis.localStorage;
    const originalWin = typeof window !== 'undefined' ? window.localStorage : undefined;

    Object.defineProperty(globalThis, 'localStorage', {
      value: this,
      writable: true,
      configurable: true,
    });
    if (typeof window !== 'undefined') {
      Object.defineProperty(window, 'localStorage', {
        value: this,
        writable: true,
        configurable: true,
      });
    }

    return () => {
      Object.defineProperty(globalThis, 'localStorage', {
        value: originalLocal,
        writable: true,
        configurable: true,
      });
      if (typeof window !== 'undefined' && originalWin !== undefined) {
        Object.defineProperty(window, 'localStorage', {
          value: originalWin,
          writable: true,
          configurable: true,
        });
      }
    };
  }
}

/**
 * MockEventSource simulates an SSE connection with controllable message dispatch,
 * errors, drops, and reconnect monitoring.
 */
export class MockEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  readonly CONNECTING = 0;
  readonly OPEN = 1;
  readonly CLOSED = 2;

  url: string;
  readyState: number = MockEventSource.CONNECTING;
  onopen: ((ev: any) => any) | null = null;
  onmessage: ((ev: any) => any) | null = null;
  onerror: ((ev: any) => any) | null = null;
  private listeners: Map<string, Set<(ev: any) => void>> = new Map();

  static instances: MockEventSource[] = [];

  constructor(url: string) {
    this.url = url;
    MockEventSource.instances.push(this);
    queueMicrotask(() => {
      if (this.readyState === MockEventSource.CONNECTING) {
        this.readyState = MockEventSource.OPEN;
        if (this.onopen) this.onopen({ type: 'open' });
      }
    });
  }

  addEventListener(type: string, listener: (ev: any) => void): void {
    if (!this.listeners.has(type)) {
      this.listeners.set(type, new Set());
    }
    this.listeners.get(type)!.add(listener);
  }

  removeEventListener(type: string, listener: (ev: any) => void): void {
    this.listeners.get(type)?.delete(listener);
  }

  dispatchEvent(event: any): boolean {
    const handlers = this.listeners.get(event.type);
    if (handlers) {
      for (const h of handlers) h(event);
    }
    return true;
  }

  emitMessage(data: any): void {
    const raw = typeof data === 'string' ? data : JSON.stringify(data);
    const event = { type: 'message', data: raw };
    if (this.onmessage) this.onmessage(event);
    this.dispatchEvent(event);
  }

  emitCustom(event: string, data: any): void {
    const raw = typeof data === 'string' ? data : JSON.stringify(data);
    const ev = { type: event, data: raw };
    this.dispatchEvent(ev);
  }

  emitError(err: any = new Error('SSE Stream Disrupted')): void {
    this.readyState = MockEventSource.CLOSED;
    if (this.onerror) this.onerror(err);
  }

  close(): void {
    this.readyState = MockEventSource.CLOSED;
  }
}

/**
 * Installs MockEventSource onto globalThis.
 */
export function setupMockEventSource(): () => void {
  // Ensure happy-dom ignores self-signed SSL certificates for ephemeral test nodes
  if (typeof window !== 'undefined' && (window as any).happyDOM?.settings?.fetch) {
    (window as any).happyDOM.settings.fetch.disableStrictSSL = true;
  }

  const original = (globalThis as any).EventSource;
  (globalThis as any).EventSource = MockEventSource;
  if (typeof window !== 'undefined') {
    (window as any).EventSource = MockEventSource;
  }
  return () => {
    (globalThis as any).EventSource = original;
    if (typeof window !== 'undefined') {
      (window as any).EventSource = original;
    }
    MockEventSource.instances = [];
  };
}

/**
 * In-memory Mock Backend Server for isolated frontend testing.
 * Intercepts global fetch when no live backend is available.
 */
export class MockBackendServer {
  devices: AudioDevice[] = [];
  rooms: RoomInfo[] = [];
  routes: MatrixRoute[] = [];
  clusterNodes: ClusterNodesResponse = {
    total_nodes: 1,
    healthy_nodes: 1,
    degraded_nodes: 0,
    timestamp: new Date().toISOString(),
    nodes: [
      {
        id: 'node-local',
        name: 'Local Node',
        api_addr: '127.0.0.1:8000',
        is_local: true,
        swim_state: 'alive',
        health_score: 100,
        health_grade: 'EXCELLENT',
        clock_sync: {
          offset_us: 0,
          drift_ppm: 0,
          sync_rtt_us: 100,
          packets_sent: 100,
          packets_lost: 0,
          is_master: true,
        },
        devices: [],
        active_streams: 0,
      },
    ],
  };

  private originalFetch: typeof globalThis.fetch | null = null;

  constructor() {
    this.resetDefaults();
  }

  resetDefaults() {
    this.devices = [
      {
        id: 'hw:0,0',
        name: 'Built-in Audio Analog Stereo',
        max_input: 2,
        max_output: 2,
        default_sample_rate: 48000,
        is_virtual: false,
        volume: 1.0,
        muted: false,
      },
      {
        id: 'hw:1,0',
        name: 'USB Studio Playout Sink',
        max_input: 0,
        max_output: 2,
        default_sample_rate: 48000,
        is_virtual: false,
        volume: 1.0,
        muted: false,
      },
      {
        id: 'hw:2,0',
        name: 'Broadcast Mic Capture Source',
        max_input: 1,
        max_output: 0,
        default_sample_rate: 48000,
        is_virtual: false,
        volume: 1.0,
        muted: false,
      },
    ];

    this.rooms = [];
    this.routes = [];
    this.clusterNodes = {
      total_nodes: 1,
      healthy_nodes: 1,
      degraded_nodes: 0,
      timestamp: new Date().toISOString(),
      nodes: [
        {
          id: 'node-local',
          name: 'Local Node',
          api_addr: '127.0.0.1:8000',
          is_local: true,
          swim_state: 'alive',
          health_score: 100,
          health_grade: 'EXCELLENT',
          clock_sync: {
            offset_us: 0,
            drift_ppm: 0,
            sync_rtt_us: 100,
            packets_sent: 100,
            packets_lost: 0,
            is_master: true,
          },
          devices: this.devices.map((d) => ({
            ...d,
            node_id: 'node-local',
            direction: d.max_input > 0 && d.max_output > 0 ? 'duplex' : d.max_input > 0 ? 'input' : 'output',
          })),
          active_streams: 0,
        },
      ],
    };
  }

  install(): () => void {
    this.originalFetch = globalThis.fetch;
    globalThis.fetch = this.handleFetch.bind(this) as any;
    if (typeof window !== 'undefined') {
      window.fetch = this.handleFetch.bind(this) as any;
    }
    return () => {
      if (this.originalFetch) {
        globalThis.fetch = this.originalFetch;
        if (typeof window !== 'undefined') {
          window.fetch = this.originalFetch;
        }
      }
    };
  }

  private async handleFetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    const rawUrl = typeof input === 'string' ? input : input instanceof URL ? input.toString() : input.url;
    const url = new URL(rawUrl, 'http://localhost:8000');
    const path = url.pathname;
    const method = (init?.method || 'GET').toUpperCase();
    const bodyText = typeof init?.body === 'string' ? init.body : '';
    const body = bodyText ? JSON.parse(bodyText) : null;

    // Helper to create Response
    const json = (data: any, status = 200) =>
      new Response(JSON.stringify(data), {
        status,
        headers: { 'Content-Type': 'application/json' },
      });

    // 1. Status
    if (path === '/api/v1/status' || path === '/api/v1/system/status' || path === '/status') {
      const statusPayload: SystemStatus = {
        status: 'ok',
        version: '1.0.0',
        uptime_seconds: 1234,
        active_routes: this.routes.length,
        discovered_peers: this.clusterNodes.total_nodes - 1,
        clock_sync_locked: true,
      };
      return json(statusPayload);
    }

    // 2. Devices
    if (path === '/api/v1/devices') {
      if (method === 'GET') {
        const kind = url.searchParams.get('kind');
        let filtered = this.devices;
        if (kind === 'input') filtered = this.devices.filter((d) => d.max_input > 0);
        else if (kind === 'output') filtered = this.devices.filter((d) => d.max_output > 0);
        else if (kind === 'virtual') filtered = this.devices.filter((d) => d.is_virtual);
        return json(filtered);
      }
    }

    if (path === '/api/v1/devices/virtual') {
      if (method === 'POST') {
        const id = `virt:${body.name.toLowerCase().replace(/\s+/g, '-')}`;
        const newDev: AudioDevice = {
          id,
          name: body.name,
          max_input: body.direction === 'output' ? 0 : body.channels,
          max_output: body.direction === 'input' ? 0 : body.channels,
          default_sample_rate: body.sample_rate || 48000,
          is_virtual: true,
          volume: 1.0,
          muted: false,
        };
        this.devices.push(newDev);
        return json(newDev, 201);
      }
    }

    if (path.startsWith('/api/v1/devices/virtual/')) {
      if (method === 'DELETE') {
        const id = decodeURIComponent(path.replace('/api/v1/devices/virtual/', ''));
        this.devices = this.devices.filter((d) => d.id !== id);
        return json({ deleted: true });
      }
    }

    if (path.startsWith('/api/v1/devices/')) {
      const id = decodeURIComponent(path.replace('/api/v1/devices/', ''));
      const dev = this.devices.find((d) => d.id === id);
      if (method === 'GET') {
        if (!dev) return json({ error: 'Device not found' }, 404);
        return json(dev);
      }
      if (method === 'PATCH') {
        if (!dev) return json({ error: 'Device not found' }, 404);
        if (typeof body?.volume === 'number') dev.volume = body.volume;
        if (typeof body?.muted === 'boolean') dev.muted = body.muted;
        return json(dev);
      }
    }

    // 3. Rooms
    if (path === '/api/v1/rooms') {
      if (method === 'GET') {
        return json(this.rooms);
      }
      if (method === 'POST') {
        const newRoom: RoomInfo = {
          id: body.id,
          name: body.name,
          state: 'idle',
          inputs: [],
          outputs: [],
          synced: false,
        };
        // Replace if already exists (for churn tests)
        this.rooms = this.rooms.filter((r) => r.id !== newRoom.id);
        this.rooms.push(newRoom);
        return json(newRoom, 201);
      }
    }

    if (path.startsWith('/api/v1/rooms/')) {
      const sub = path.replace('/api/v1/rooms/', '');
      const parts = sub.split('/').map(decodeURIComponent);
      const roomId = parts[0];
      const room = this.rooms.find((r) => r.id === roomId);

      if (parts.length === 1) {
        if (method === 'GET') {
          if (!room) return json({ error: 'Room not found' }, 404);
          return json(room);
        }
        if (method === 'DELETE') {
          this.rooms = this.rooms.filter((r) => r.id !== roomId);
          return json({ deleted: true });
        }
      }

      if (room && parts[1] === 'inputs') {
        if (method === 'POST') {
          const devId = body.node_id ? `${body.node_id}/${body.device_id}` : body.device_id;
          if (!room.inputs.includes(devId)) {
            room.inputs.push(devId);
          }
          return json(room);
        }
        if (method === 'DELETE' && parts[2]) {
          const devId = parts.slice(2).join('/');
          const clean = devId.includes('/') ? devId.slice(devId.indexOf('/') + 1) : devId;
          room.inputs = room.inputs.filter((id) => {
            if (id === devId) return false;
            if (devId.includes('/')) return true;
            return id !== clean && !id.endsWith(`/${clean}`);
          });
          return json(room);
        }
      }

      if (room && parts[1] === 'outputs') {
        if (method === 'POST') {
          const devId = body.node_id ? `${body.node_id}/${body.device_id}` : body.device_id;
          if (!room.outputs.includes(devId)) {
            room.outputs.push(devId);
          }
          return json(room);
        }
        if (method === 'DELETE' && parts[2]) {
          const devId = parts.slice(2).join('/');
          const clean = devId.includes('/') ? devId.slice(devId.indexOf('/') + 1) : devId;
          room.outputs = room.outputs.filter((id) => {
            if (id === devId) return false;
            if (devId.includes('/')) return true;
            return id !== clean && !id.endsWith(`/${clean}`);
          });
          return json(room);
        }
      }

      if (room && parts[1] === 'sync' && method === 'POST') {
        room.synced = !!body.synced;
        return json(room);
      }

      if (room && parts[1] === 'bindings' && method === 'PUT') {
        room.inputs = body.inputs || [];
        room.outputs = body.outputs || [];
        if (body.synced !== undefined) room.synced = body.synced;
        return json(room);
      }
    }

    // 4. Matrix Routes
    if (path === '/api/v1/routes') {
      if (method === 'GET') {
        return json(this.routes);
      }
      if (method === 'POST') {
        const id = body.id || `route-${Date.now()}-${Math.floor(Math.random() * 1000)}`;
        const inCh = body.source?.channels?.length || 2;
        const outCh = body.destination?.channels?.length || 2;
        const newRoute: MatrixRoute = {
          id,
          name: body.name || `${body.source.device_id} -> ${body.destination.device_id}`,
          source: body.source,
          destination: body.destination,
          in_channels: inCh,
          out_channels: outCh,
          coefficients: body.coefficients || new Array(inCh * outCh).fill(1.0),
          slew_ms: body.slew_ms || 20.0,
          muted: false,
          state: 'active',
        };
        this.routes.push(newRoute);
        return json(newRoute, 201);
      }
    }

    if (path.startsWith('/api/v1/routes/')) {
      const sub = path.replace('/api/v1/routes/', '');
      const parts = sub.split('/').map(decodeURIComponent);
      const routeId = parts[0];
      const route = this.routes.find((r) => r.id === routeId);

      if (parts.length === 1) {
        if (method === 'GET') {
          if (!route) return json({ error: 'Route not found' }, 404);
          return json(route);
        }
        if (method === 'DELETE') {
          this.routes = this.routes.filter((r) => r.id !== routeId);
          return json({ deleted: true });
        }
        if (method === 'PATCH') {
          if (!route) return json({ error: 'Route not found' }, 404);
          if (body.coefficients) route.coefficients = body.coefficients;
          if (body.slew_ms !== undefined) route.slew_ms = body.slew_ms;
          if (body.muted !== undefined) route.muted = body.muted;
          return json(route);
        }
      }

      if (route && parts[1] === 'crosspoint' && method === 'POST') {
        const { out_channel, in_channel, gain, slew_ms } = body;
        const inCh = route.in_channels || 2;
        const idx = out_channel * inCh + in_channel;
        if (idx >= 0 && idx < route.coefficients.length) {
          route.coefficients[idx] = gain;
        }
        if (slew_ms !== undefined) route.slew_ms = slew_ms;
        return json(route);
      }
    }

    // 5. Cluster Nodes & Devices
    if (path === '/api/v1/cluster/nodes') {
      return json(this.clusterNodes);
    }

    if (path === '/api/v1/cluster/devices') {
      const allDevs: AudioDeviceSummary[] = [];
      for (const node of this.clusterNodes.nodes) {
        for (const dev of node.devices) {
          allDevs.push(dev);
        }
      }
      return json({
        timestamp: new Date().toISOString(),
        devices: allDevs,
      } as ClusterDevicesResponse);
    }

    // 6. Network health & telemetry
    if (path === '/api/v1/network/health') {
      return json({
        node_id: 'node-local',
        timestamp: new Date().toISOString(),
        health_score: 98.5,
        health_grade: 'EXCELLENT',
        cluster: {
          active_members: this.clusterNodes.total_nodes,
          suspect_members: 0,
          dead_members: 0,
          lan_beacon_active: true,
          beacons_sent: 100,
          beacons_received: 100,
          token_rejections: 0,
          seed_retry_active: false,
          seed_join_attempts: 1,
          seed_join_successes: 1,
        },
        sync: {
          offset_us: 0,
          drift_ppm: 0,
          sync_rtt_us: 50,
          packets_sent: 100,
          packets_lost: 0,
          is_master: true,
        },
        latency_matrix: {},
        peers: {},
        local_underruns: 0,
      });
    }

    // Fallback 404
    return json({ error: `Not Found: ${method} ${path}` }, 404);
  }
}

/**
 * Calculates audio edge styling and dynamic decibel interpolation
 * identical to WebUI canvas edge and matrix rendering.
 */
export function computeEdgeStyle(linearGain: number): {
  strokeWidth: string;
  strokeColor: string;
  gainDb: number;
} {
  const gainDb = linearGain <= 0.00001 ? -100 : 20 * Math.log10(linearGain);
  const strokeWidth = `${Math.min(6, Math.max(1.5, 2 + linearGain * 2)).toFixed(1)}px`;
  const strokeColor = gainDb > 0 ? '#f59e0b' : '#06b6d4';
  return { strokeWidth, strokeColor, gainDb };
}
