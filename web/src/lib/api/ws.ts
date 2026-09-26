import type { MeterFrame, CrosspointGainRequest } from './types';

export type WSConnectionState = 'connected' | 'connecting' | 'disconnected' | 'error';

export interface WSOptions {
  baseUrl?: string;
  token?: string;
  reconnectIntervalMs?: number;
  maxReconnectIntervalMs?: number;
}

export type WSEventHandler<T = unknown> = (data: T) => void;

export interface WSCrosspointMessage {
  type: 'crosspoint';
  route_id: string;
  crosspoint: CrosspointGainRequest;
}

export interface WSMuteMessage {
  type: 'mute';
  route_id: string;
  muted: boolean;
}

export type WSOutboundMessage = WSCrosspointMessage | WSMuteMessage | { type: string; [key: string]: unknown };

export class TelemetryWebSocket {
  private ws: WebSocket | null = null;
  private baseUrl: string = '';
  private token: string = '';
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private reconnectIntervalMs: number = 1000;
  private maxReconnectIntervalMs: number = 10000;
  private currentBackoff: number = 1000;
  private isExplicitlyClosed: boolean = false;
  private listeners: Map<string, Set<WSEventHandler>> = new Map();
  private stateChangeListeners: Set<(state: WSConnectionState) => void> = new Set();
  public state: WSConnectionState = 'disconnected';

  constructor(options: WSOptions = {}) {
    this.baseUrl = (options.baseUrl || '').replace(/\/+$/, '');
    this.token = options.token || '';
    this.reconnectIntervalMs = options.reconnectIntervalMs || 1000;
    this.maxReconnectIntervalMs = options.maxReconnectIntervalMs || 10000;
    this.currentBackoff = this.reconnectIntervalMs;
  }

  setBaseUrl(url: string) {
    this.baseUrl = url.replace(/\/+$/, '');
    if (this.ws) {
      this.reconnect();
    }
  }

  setToken(token: string) {
    this.token = token;
    if (this.ws) {
      this.reconnect();
    }
  }

  private constructWsUrl(): string {
    let wsHost = this.baseUrl;
    if (!wsHost) {
      if (typeof window !== 'undefined') {
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        wsHost = `${proto}//${window.location.host}`;
      } else {
        wsHost = 'ws://localhost:8000';
      }
    } else {
      if (wsHost.startsWith('https://')) {
        wsHost = wsHost.replace('https://', 'wss://');
      } else if (wsHost.startsWith('http://')) {
        wsHost = wsHost.replace('http://', 'ws://');
      }
    }

    const params = new URLSearchParams();
    if (this.token) params.set('token', this.token);
    const qs = params.toString() ? `?${params.toString()}` : '';

    return `${wsHost}/api/v1/telemetry/ws${qs}`;
  }

  public connect(): void {
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) {
      return;
    }

    this.isExplicitlyClosed = false;
    this.setState('connecting');

    try {
      const url = this.constructWsUrl();
      this.ws = new WebSocket(url);

      this.ws.onopen = () => {
        this.currentBackoff = this.reconnectIntervalMs;
        this.setState('connected');
        this.emit('open', null);
      };

      this.ws.onmessage = (event: MessageEvent) => {
        try {
          if (typeof event.data === 'string') {
            const parsed = JSON.parse(event.data);
            if (parsed.type) {
              this.emit(parsed.type, parsed.data !== undefined ? parsed.data : parsed);
            } else if (parsed.stream_id && parsed.channels) {
              this.emit('meter', parsed as MeterFrame);
            } else {
              this.emit('message', parsed);
            }
          }
        } catch {
          this.emit('message', event.data);
        }
      };

      this.ws.onerror = (err) => {
        this.setState('error');
        this.emit('error', err);
      };

      this.ws.onclose = () => {
        this.ws = null;
        this.setState('disconnected');
        if (!this.isExplicitlyClosed) {
          this.scheduleReconnect();
        }
      };
    } catch (err) {
      this.setState('error');
      this.scheduleReconnect();
    }
  }

  public disconnect(): void {
    this.isExplicitlyClosed = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.close();
      this.ws = null;
    }
    this.setState('disconnected');
  }

  public reconnect(): void {
    this.disconnect();
    this.isExplicitlyClosed = false;
    this.connect();
  }

  private scheduleReconnect(): void {
    if (this.reconnectTimer || this.isExplicitlyClosed) return;

    this.setState('connecting');
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, this.currentBackoff);

    this.currentBackoff = Math.min(this.currentBackoff * 1.5, this.maxReconnectIntervalMs);
  }

  private setState(newState: WSConnectionState): void {
    if (this.state !== newState) {
      this.state = newState;
      for (const listener of this.stateChangeListeners) {
        listener(newState);
      }
    }
  }

  public onStateChange(listener: (state: WSConnectionState) => void): () => void {
    this.stateChangeListeners.add(listener);
    listener(this.state);
    return () => this.stateChangeListeners.delete(listener);
  }

  public on<T = unknown>(eventName: string, handler: WSEventHandler<T>): () => void {
    if (!this.listeners.has(eventName)) {
      this.listeners.set(eventName, new Set());
    }
    const set = this.listeners.get(eventName)!;
    set.add(handler as WSEventHandler);
    return () => set.delete(handler as WSEventHandler);
  }

  public onMeter(handler: (frame: MeterFrame) => void): () => void {
    return this.on<MeterFrame>('meter', handler);
  }

  public send(message: WSOutboundMessage): boolean {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(message));
      return true;
    }
    return false;
  }

  public sendCrosspoint(
    routeId: string,
    outChannel: number,
    inChannel: number,
    gain: number,
    slewMs: number = 20.0
  ): boolean {
    return this.send({
      type: 'crosspoint',
      route_id: routeId,
      crosspoint: {
        out_channel: outChannel,
        in_channel: inChannel,
        gain,
        slew_ms: slewMs,
      },
    });
  }

  public sendMute(routeId: string, muted: boolean): boolean {
    return this.send({
      type: 'mute',
      route_id: routeId,
      muted,
    });
  }

  private emit(eventName: string, data: unknown): void {
    const handlers = this.listeners.get(eventName);
    if (handlers) {
      for (const handler of handlers) {
        try {
          handler(data);
        } catch (err) {
          console.error(`Error in WebSocket listener for "${eventName}":`, err);
        }
      }
    }
  }
}
